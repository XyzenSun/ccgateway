package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Server 聚合 HTTP 路由、配置快照、SQLite 存储和异步日志队列。
type Server struct {
	store       *Store
	snapshot    atomic.Value
	logger      *LogWriter
	mux         *http.ServeMux
	httpClient  *http.Client
	listenAddr  string
	adminCookie string
}

// NewServer 初始化数据库、加载配置快照、启动日志 writer 并装配 HTTP 路由。
func NewServer(ctx context.Context, runtimeConfig RuntimeConfig) (*Server, error) {
	store, err := OpenStore(ctx, runtimeConfig)
	if err != nil {
		return nil, err
	}
	snapshot, err := store.LoadSnapshot(ctx)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	server := &Server{
		store:       store,
		logger:      NewLogWriter(store, snapshot.Settings.LogQueueSize),
		mux:         http.NewServeMux(),
		httpClient:  &http.Client{Timeout: 0},
		listenAddr:  snapshot.Settings.ListenAddress,
		adminCookie: "cc_gateway_admin",
	}
	server.snapshot.Store(snapshot)
	server.routes()
	server.logger.Start(ctx)
	go server.cleanupLoop(ctx)
	return server, nil
}

// Handler 返回 HTTP handler，main 使用它启动标准库 HTTP server。
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAddress 返回当前监听地址，启动地址来自数据库初始化后的 settings。
func (s *Server) ListenAddress() string { return s.listenAddr }

// Close 关闭后台资源，日志 writer 使用上下文结束退出。
func (s *Server) Close() error { return s.store.Close() }

// routes 注册所有入口，后台 API 单独包一层认证中间件以保持认证边界清晰。
func (s *Server) routes() {
	s.mux.HandleFunc("/v1/", s.handleGateway)
	s.mux.HandleFunc("/admin", s.handleAdminPage)
	s.mux.HandleFunc("/logs", s.handleAdminPage)
	s.mux.HandleFunc("/_admin/static/", serveAdminStatic)
	s.mux.HandleFunc("/_admin/api/login", s.handleAdminLogin)
	s.mux.HandleFunc("/_admin/api/", s.requireAdmin(s.handleAdminAPI))
}

// currentSnapshot 返回最新配置快照，请求路径只读该快照而不查询 SQLite。
func (s *Server) currentSnapshot() ConfigSnapshot {
	return s.snapshot.Load().(ConfigSnapshot)
}

// refreshSnapshot 在管理配置写入成功后刷新内存快照，失败会让调用方返回错误。
func (s *Server) refreshSnapshot(ctx context.Context) error {
	snapshot, err := s.store.LoadSnapshot(ctx)
	if err != nil {
		return err
	}
	s.snapshot.Store(snapshot)
	return nil
}

// handleGateway 处理 /v1/* 代理入口，包含 API Key 认证、模型改写、上游选择和日志提交。
func (s *Server) handleGateway(w http.ResponseWriter, r *http.Request) {
	started := time.Now().UTC()
	snapshot := s.currentSnapshot()
	group, ok := findGroupForRequest(r, snapshot)
	if !ok || !group.Enabled {
		writeJSONError(w, http.StatusUnauthorized, "invalid gateway api key")
		return
	}

	if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
		s.handleModels(w, group, snapshot)
		return
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	_ = r.Body.Close()

	clientModel, upstreamModel, selectedUpstreamID, rewrittenBody, err := rewriteRequestBody(requestBody, group, snapshot)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	upstream, err := selectUpstream(group, selectedUpstreamID, snapshot)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	targetURL, err := buildTargetURL(upstream.BaseURL, r.URL)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "invalid upstream url")
		return
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(rewrittenBody))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "failed to create upstream request")
		return
	}
	copyHeaders(outReq.Header, r.Header)
	outReq.Header.Del("Authorization")
	outReq.Header.Del("x-api-key")
	applyUpstreamAuth(outReq.Header, upstream)

	resp, err := s.httpClient.Do(outReq)
	logRecord := baseRequestLog(started, r, targetURL, group, upstream, clientModel, upstreamModel, requestBody, snapshot.Settings)
	if err != nil {
		logRecord.CompletedAt = time.Now().UTC()
		logRecord.DurationMS = logRecord.CompletedAt.Sub(started).Milliseconds()
		logRecord.StatusCode = http.StatusBadGateway
		logRecord.Error = err.Error()
		s.logger.Enqueue(logRecord)
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	needRewrite := snapshot.Settings.RewriteModelInResponse &&
		clientModel != "" && upstreamModel != "" && clientModel != upstreamModel

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	// 非流式回写时替换 model 会改变 body 长度，需删除上游的 Content-Length 让 HTTP 库自动处理。
	if needRewrite && !isSSE {
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.StatusCode)

	responseCapture := &limitedBuffer{limit: snapshot.Settings.MaxResponseBodyBytes}
	logFull := snapshot.Settings.LogMode == LogModeFull

	var written int64
	var copyErr error
	if needRewrite && isSSE {
		written, copyErr = copySSEWithModelRewrite(w, resp.Body, responseCapture, logFull, upstreamModel, clientModel)
	} else if needRewrite {
		written, copyErr = copyJSONWithModelRewrite(w, resp.Body, responseCapture, logFull, upstreamModel, clientModel)
	} else {
		writer := io.Writer(w)
		if logFull {
			writer = io.MultiWriter(w, responseCapture)
		}
		written, copyErr = io.Copy(writer, resp.Body)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	logRecord.CompletedAt = time.Now().UTC()
	logRecord.DurationMS = logRecord.CompletedAt.Sub(started).Milliseconds()
	logRecord.StatusCode = resp.StatusCode
	logRecord.IsStream = isSSE
	logRecord.ResponseBytes = written
	logRecord.ResponseBodyTruncated = responseCapture.truncated
	if copyErr != nil {
		logRecord.Error = copyErr.Error()
	}
	if snapshot.Settings.LogMode == LogModeFull {
		headers := headersJSON(resp.Header)
		logRecord.ResponseHeadersJSON = &headers
		logRecord.ResponseBody = responseCapture.bytes()
		if logRecord.IsStream {
			mode := "sse"
			logRecord.ResponseBodyMode = &mode
		} else {
			mode := "text"
			logRecord.ResponseBodyMode = &mode
		}
	}
	s.logger.Enqueue(logRecord)
}

// handleModels 返回当前 API Key group 可见的 client_model 列表，不暴露上游真实模型名。
func (s *Server) handleModels(w http.ResponseWriter, group APIKeyGroup, snapshot ConfigSnapshot) {
	mappings := snapshot.MappingsByGroup[group.ID]
	items := make([]map[string]any, 0, len(mappings))
	for clientModel := range mappings {
		items = append(items, map[string]any{"type": "model", "id": clientModel, "display_name": clientModel})
	}
	writeJSON(w, http.StatusOK, map[string]any{"type": "list", "data": items})
}

// findGroupForRequest 支持 Authorization Bearer 和 x-api-key 双 header，任一匹配即可通过。
func findGroupForRequest(r *http.Request, snapshot ConfigSnapshot) (APIKeyGroup, bool) {
	keys := []string{}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		keys = append(keys, strings.TrimSpace(auth[7:]))
	}
	if apiKey := r.Header.Get("x-api-key"); apiKey != "" {
		keys = append(keys, strings.TrimSpace(apiKey))
	}
	for _, key := range keys {
		if group, ok := snapshot.GroupsByAPIKey[key]; ok {
			return group, true
		}
	}
	return APIKeyGroup{}, false
}

// rewriteRequestBody 在 JSON 顶层 model 存在时执行模型映射，没有 model 的请求保持原 body 透传。
func rewriteRequestBody(body []byte, group APIKeyGroup, snapshot ConfigSnapshot) (string, string, string, []byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return "", "", "", body, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", "", body, nil
	}
	modelValue, hasModel := payload["model"]
	if !hasModel {
		return "", "", "", body, nil
	}
	clientModel, ok := modelValue.(string)
	if !ok || clientModel == "" {
		return "", "", "", nil, errors.New("model must be a string")
	}
	mapping, ok := snapshot.MappingsByGroup[group.ID][clientModel]
	if !ok || !mapping.Enabled {
		return "", "", "", nil, fmt.Errorf("model %s is not mapped for this api key group", clientModel)
	}
	payload["model"] = mapping.UpstreamModel
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return "", "", "", nil, err
	}
	return clientModel, mapping.UpstreamModel, mapping.UpstreamID, rewritten, nil
}

// selectUpstream 按映射覆盖上游优先、group 默认上游兜底的规则选择上游。
func selectUpstream(group APIKeyGroup, mappingUpstreamID string, snapshot ConfigSnapshot) (Upstream, error) {
	upstreamID := group.DefaultUpstreamID
	if mappingUpstreamID != "" {
		upstreamID = mappingUpstreamID
	}
	upstream, ok := snapshot.Upstreams[upstreamID]
	if !ok || !upstream.Enabled {
		return Upstream{}, errors.New("selected upstream is missing or disabled")
	}
	return upstream, nil
}

// buildTargetURL 将请求路径和 query 拼到上游 base_url 后，保留任意 /v1/* 新接口透传能力。
func buildTargetURL(baseURL string, requestURL *url.URL) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	parsed.Path = basePath + requestURL.Path
	parsed.RawQuery = requestURL.RawQuery
	return parsed.String(), nil
}

// applyUpstreamAuth 根据上游认证策略写入新的认证请求头，避免网关 API Key 泄露给上游。
func applyUpstreamAuth(header http.Header, upstream Upstream) {
	switch upstream.AuthType {
	case AuthTypeAuthorizationBearer:
		header.Set("Authorization", "Bearer "+upstream.APIKey)
	case AuthTypeCustomHeader:
		value := upstream.APIKey
		if upstream.AuthHeaderValueTemplate != "" {
			value = strings.ReplaceAll(upstream.AuthHeaderValueTemplate, "{{api_key}}", upstream.APIKey)
		}
		header.Set(upstream.AuthHeaderName, value)
	default:
		header.Set("x-api-key", upstream.APIKey)
	}
}

// copyHeaders 按最小变更策略复制客户端请求头，Host 和 Content-Length 交给标准库处理。
func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// baseRequestLog 构造请求开始阶段可确定的日志字段，日志模式决定是否保存 request payload。
func baseRequestLog(started time.Time, r *http.Request, targetURL string, group APIKeyGroup, upstream Upstream, clientModel string, upstreamModel string, body []byte, settings Settings) RequestLog {
	requestCapture := &limitedBuffer{limit: settings.MaxRequestBodyBytes}
	_, _ = requestCapture.Write(body)
	logRecord := RequestLog{
		ID:                  newID("log"),
		StartedAt:           started,
		Method:              r.Method,
		Path:                r.URL.RequestURI(),
		TargetURL:           targetURL,
		APIKeyGroupID:       group.ID,
		UpstreamID:          upstream.ID,
		UpstreamHost:        hostOnly(upstream.BaseURL),
		ClientModel:         clientModel,
		UpstreamModel:       upstreamModel,
		ClaudeSessionID:     r.Header.Get("X-Claude-Code-Session-Id"),
		ClaudeAgentID:       r.Header.Get("X-Claude-Code-Agent-Id"),
		ClaudeParentAgentID: r.Header.Get("X-Claude-Code-Parent-Agent-Id"),
		RequestBytes:        int64(len(body)),
		LogMode:             settings.LogMode,
		SummaryJSON:         "{}",
	}
	if settings.LogMode == LogModeFull {
		headers := headersJSON(r.Header)
		logRecord.RequestHeadersJSON = &headers
		logRecord.RequestBody = requestCapture.bytes()
		logRecord.RequestBodyTruncated = requestCapture.truncated
	}
	return logRecord
}

// hostOnly 从 URL 中提取 host，失败时返回原值方便排查坏配置。
func hostOnly(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return parsed.Host
}

// headersJSON 将请求头编码成 JSON，日志失败不能影响请求所以失败时返回空对象。
func headersJSON(header http.Header) string {
	encoded, err := json.Marshal(header)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// writeJSONError 以 Anthropic 风格的 error envelope 返回网关错误。
func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": message}})
}

// writeJSON 写 JSON 响应，后台和网关错误都使用它统一 Content-Type。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("写入 JSON 响应失败: %v", err)
	}
}

// copySSEWithModelRewrite 逐行读取 SSE 流，capture 接收原始字节用于日志，客户端接收替换后的字节。
// 在第一个包含 model 字段的事件完成替换后切换到 io.Copy 批量传输剩余事件。
func copySSEWithModelRewrite(w io.Writer, body io.Reader, capture *limitedBuffer, logFull bool, oldModel, newModel string) (int64, error) {
	oldPattern := []byte(`"model":"` + oldModel + `"`)
	newPattern := []byte(`"model":"` + newModel + `"`)
	oldPatternSpaced := []byte(`"model": "` + oldModel + `"`)
	newPatternSpaced := []byte(`"model": "` + newModel + `"`)

	reader := bufio.NewReaderSize(body, 4096)
	var written int64
	replaced := false

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			// 日志始终接收原始字节
			if logFull {
				_, _ = capture.Write(line)
			}

			clientLine := line
			if !replaced {
				if bytes.Contains(line, oldPattern) {
					clientLine = bytes.Replace(line, oldPattern, newPattern, 1)
					replaced = true
				} else if bytes.Contains(line, oldPatternSpaced) {
					clientLine = bytes.Replace(line, oldPatternSpaced, newPatternSpaced, 1)
					replaced = true
				}
			}

			n, wErr := w.Write(clientLine)
			written += int64(n)
			if wErr != nil {
				return written, wErr
			}

			// 替换完成后，切换到批量传输剩余数据
			if replaced {
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				var tailWriter io.Writer = w
				if logFull {
					tailWriter = io.MultiWriter(w, capture)
				}
				// 先把 bufio 中已缓冲的数据传输出去
				if reader.Buffered() > 0 {
					buffered := make([]byte, reader.Buffered())
					n, _ := reader.Read(buffered)
					if n > 0 {
						nw, wErr := tailWriter.Write(buffered[:n])
						written += int64(nw)
						if wErr != nil {
							return written, wErr
						}
					}
				}
				n64, copyErr := io.Copy(tailWriter, body)
				written += n64
				return written, copyErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return written, nil
			}
			return written, err
		}
	}
}

// copyJSONWithModelRewrite 读取完整非流式响应，capture 接收原始字节，客户端接收替换后的字节。
func copyJSONWithModelRewrite(w io.Writer, body io.Reader, capture *limitedBuffer, logFull bool, oldModel, newModel string) (int64, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return 0, err
	}
	// 日志始终接收原始字节
	if logFull {
		_, _ = capture.Write(data)
	}

	oldPattern := []byte(`"model":"` + oldModel + `"`)
	newPattern := []byte(`"model":"` + newModel + `"`)
	oldPatternSpaced := []byte(`"model": "` + oldModel + `"`)
	newPatternSpaced := []byte(`"model": "` + newModel + `"`)

	clientData := data
	if bytes.Contains(data, oldPattern) {
		clientData = bytes.Replace(data, oldPattern, newPattern, 1)
	} else if bytes.Contains(data, oldPatternSpaced) {
		clientData = bytes.Replace(data, oldPatternSpaced, newPatternSpaced, 1)
	}

	n, wErr := w.Write(clientData)
	return int64(n), wErr
}

// limitedBuffer 是日志 payload 捕获缓冲，超过限制后截断并继续计数响应流。
type limitedBuffer struct {
	limit     int64
	buf       bytes.Buffer
	truncated bool
}

// Write 保存不超过 limit 的前缀，limit 为 0 时只标记截断不保存 payload。
func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		if len(p) > 0 {
			b.truncated = true
		}
		return len(p), nil
	}
	remaining := b.limit - int64(b.buf.Len())
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

// bytes 返回已捕获 payload 的拷贝，避免后续写入影响日志对象。
func (b *limitedBuffer) bytes() []byte { return append([]byte(nil), b.buf.Bytes()...) }

// cleanupLoop 按配置周期清理日志，清理失败只记录内部日志而不影响代理服务。
func (s *Server) cleanupLoop(ctx context.Context) {
	for {
		snapshot := s.currentSnapshot()
		interval := time.Duration(snapshot.Settings.LogCleanupIntervalMinutes) * time.Minute
		if interval < time.Minute {
			interval = time.Minute
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if err := s.store.CleanupLogs(context.Background(), snapshot.Settings); err != nil {
				log.Printf("日志清理失败: %v", err)
			}
		}
	}
}
