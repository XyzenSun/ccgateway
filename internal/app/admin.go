package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// adminSession 保存管理登录态与过期时间，cookie 值本身是随机 token。
type adminSession struct {
	Token     string
	ExpiresAt time.Time
}

var activeAdminSession atomicAdminSession

// atomicAdminSession 用 atomic.Value 保存单机后台登录态，重启后要求重新登录更安全也更简单。
type atomicAdminSession struct{ value atomicValue }

// atomicValue 是 atomic.Value 的窄封装，避免在其他文件散落类型断言。
type atomicValue struct{ v atomic.Value }

// Store 写入当前管理登录态。
func (a *atomicAdminSession) Store(session adminSession) { a.value.v.Store(session) }

// Load 读取当前管理登录态，没有登录态时返回 false。
func (a *atomicAdminSession) Load() (adminSession, bool) {
	value := a.value.v.Load()
	if value == nil {
		return adminSession{}, false
	}
	return value.(adminSession), true
}

// handleAdminPage 返回内嵌后台单页，未登录时页面中的前端会展示登录框。
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin" && r.URL.Path != "/logs" {
		http.NotFound(w, r)
		return
	}
	serveAdminHTML(w, r)
}

// handleAdminLogin 校验管理密钥并设置 HttpOnly cookie，后台密钥与网关 API Key 完全分离。
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload struct {
		AdminKey string `json:"admin_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	settings, err := s.store.LoadSettings(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to load settings")
		return
	}
	if !VerifySecret(payload.AdminKey, settings.AdminKeyHash) {
		writeJSONError(w, http.StatusUnauthorized, "invalid admin key")
		return
	}
	token, err := randomToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	session := adminSession{Token: token, ExpiresAt: time.Now().UTC().Add(24 * time.Hour)}
	activeAdminSession.Store(session)
	http.SetCookie(w, &http.Cookie{Name: s.adminCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// requireAdmin 保护管理 API，所有配置写入与日志查看都要求管理登录态。
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(s.adminCookie)
		if err != nil || cookie.Value == "" {
			writeJSONError(w, http.StatusUnauthorized, "admin login required")
			return
		}
		session, ok := activeAdminSession.Load()
		if !ok || time.Now().UTC().After(session.ExpiresAt) || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(session.Token)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "admin login required")
			return
		}
		next(w, r)
	}
}

// handleAdminAPI 分发同源后台 API，路由风格保持简单以适配原生前端。
func (s *Server) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/_admin/api")
	switch {
	case path == "/dashboard" && r.Method == http.MethodGet:
		s.handleDashboard(w, r)
	case path == "/upstreams" && r.Method == http.MethodPost:
		s.handleSaveUpstream(w, r, "")
	case strings.HasPrefix(path, "/upstreams/"):
		s.handleUpstreamByID(w, r, strings.TrimPrefix(path, "/upstreams/"))
	case path == "/groups" && r.Method == http.MethodPost:
		s.handleSaveGroup(w, r, "")
	case strings.HasPrefix(path, "/groups/") && strings.HasSuffix(path, "/mappings") && r.Method == http.MethodPost:
		groupID := strings.TrimSuffix(strings.TrimPrefix(path, "/groups/"), "/mappings")
		s.handleSaveMapping(w, r, "", groupID)
	case strings.HasPrefix(path, "/groups/"):
		s.handleGroupByID(w, r, strings.TrimPrefix(path, "/groups/"))
	case strings.HasPrefix(path, "/mappings/"):
		s.handleMappingByID(w, r, strings.TrimPrefix(path, "/mappings/"))
	case path == "/settings/logging" && r.Method == http.MethodPatch:
		s.handleLoggingSettings(w, r)
	case path == "/admin-key" && r.Method == http.MethodPost:
		s.handleAdminKey(w, r)
	case path == "/logs" && r.Method == http.MethodGet:
		s.handleLogs(w, r)
	case strings.HasPrefix(path, "/logs/") && r.Method == http.MethodGet:
		s.handleLogDetail(w, r, strings.TrimPrefix(path, "/logs/"))
	case path == "/logs/cleanup" && r.Method == http.MethodPost:
		s.handleCleanupLogs(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "admin api not found")
	}
}

// handleDashboard 返回后台首页聚合数据。
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.Dashboard(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// handleUpstreamByID 处理 upstream 的 PATCH 和 DELETE。
func (s *Server) handleUpstreamByID(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodPatch:
		s.handleSaveUpstream(w, r, id)
	case http.MethodDelete:
		if err := s.store.DeleteUpstream(r.Context(), id); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.refreshSnapshot(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleSaveUpstream 保存 upstream 并刷新快照，API Key 为空时更新路径保留旧值。
func (s *Server) handleSaveUpstream(w http.ResponseWriter, r *http.Request, id string) {
	var upstream Upstream
	if err := decodeJSON(r, &upstream); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	upstream.ID = id
	saved, err := s.store.SaveUpstream(r.Context(), upstream)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.refreshSnapshot(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// handleGroupByID 处理 API Key group 的 PATCH 和 DELETE。
func (s *Server) handleGroupByID(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodPatch:
		s.handleSaveGroup(w, r, id)
	case http.MethodDelete:
		if err := s.store.DeleteGroup(r.Context(), id); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.refreshSnapshot(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleSaveGroup 保存 API Key group，并立即刷新代理请求使用的快照。
func (s *Server) handleSaveGroup(w http.ResponseWriter, r *http.Request, id string) {
	var group APIKeyGroup
	if err := decodeJSON(r, &group); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	group.ID = id
	saved, err := s.store.SaveGroup(r.Context(), group)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.refreshSnapshot(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// handleMappingByID 处理模型映射的 PATCH 和 DELETE。
func (s *Server) handleMappingByID(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodPatch:
		s.handleSaveMapping(w, r, id, "")
	case http.MethodDelete:
		if err := s.store.DeleteMapping(r.Context(), id); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.refreshSnapshot(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleSaveMapping 保存单条模型映射，groupID 参数来自嵌套路由时优先使用。
func (s *Server) handleSaveMapping(w http.ResponseWriter, r *http.Request, id string, groupID string) {
	var mapping ModelMapping
	if err := decodeJSON(r, &mapping); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	mapping.ID = id
	if groupID != "" {
		mapping.GroupID = groupID
	}
	saved, err := s.store.SaveMapping(r.Context(), mapping)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.refreshSnapshot(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// handleLoggingSettings 保存日志设置并刷新快照，新的日志模式对后续请求立即生效。
func (s *Server) handleLoggingSettings(w http.ResponseWriter, r *http.Request) {
	var settings Settings
	if err := decodeJSON(r, &settings); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SaveLoggingSettings(r.Context(), settings); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.refreshSnapshot(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminKey 修改管理密钥，保存 hash 后当前登录态仍保留以避免保存后立即打断操作。
func (s *Server) handleAdminKey(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		AdminKey string `json:"admin_key"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(payload.AdminKey) == "" {
		writeJSONError(w, http.StatusBadRequest, "admin key cannot be empty")
		return
	}
	hash, err := HashSecret(payload.AdminKey)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.SaveAdminKeyHash(r.Context(), hash); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.refreshSnapshot(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleLogs 查询日志列表，分页参数在 Store 层统一限制。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := LogFilter{
		Page:          atoiDefault(query.Get("page"), 1),
		PageSize:      atoiDefault(query.Get("page_size"), 25),
		Search:        query.Get("search"),
		StartedAfter:  query.Get("started_after"),
		StartedBefore: query.Get("started_before"),
		StatusCode:    query.Get("status_code"),
		Path:          query.Get("path"),
		ClientModel:   query.Get("client_model"),
		UpstreamModel: query.Get("upstream_model"),
		UpstreamID:    query.Get("upstream_id"),
		GroupID:       query.Get("group_id"),
		SessionID:     query.Get("session_id"),
		AgentID:       query.Get("agent_id"),
		IsStream:      query.Get("is_stream"),
		Truncated:     query.Get("truncated"),
	}
	logs, err := s.store.ListLogs(r.Context(), filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, logQueryError("查询", err).Error())
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

// handleLogDetail 返回单条日志详情，不存在时返回 404 便于前端显示已清理状态。
func (s *Server) handleLogDetail(w http.ResponseWriter, r *http.Request, id string) {
	detail, err := s.store.GetLogDetail(r.Context(), id)
	if err != nil {
		if noRowsError(err) {
			writeJSONError(w, http.StatusNotFound, "log not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleCleanupLogs 手动触发日志清理，使用当前 settings 的两种策略。
func (s *Server) handleCleanupLogs(w http.ResponseWriter, r *http.Request) {
	settings := s.currentSnapshot().Settings
	if err := s.store.CleanupLogs(context.Background(), settings); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// decodeJSON 解码后台 JSON 请求并拒绝空 body，避免 PATCH 写入零值误改配置。
func decodeJSON(r *http.Request, target any) error {
	if r.Body == nil {
		return errors.New("request body is required")
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

// randomToken 生成管理登录 cookie token，token 不持久化，服务重启自然失效。
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
