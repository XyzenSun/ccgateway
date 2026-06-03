package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store 封装 SQLite 长连接与所有持久化操作，配置写入使用事务，日志写入保持轻量直接追加。
type Store struct {
	db     *sql.DB
	dbPath string
}

// LoadRuntimeConfig 从环境变量读取启动配置，环境变量只覆盖进程入口，不直接改写数据库配置。
func LoadRuntimeConfig() RuntimeConfig {
	config := RuntimeConfig{
		SQLitePath:    getenvDefault("SQLITE_PATH", DefaultSQLitePath),
		ListenAddress: getenvDefault("LISTEN_ADDR", DefaultListenAddress),
		AdminKey:      os.Getenv("ADMIN_KEY"),
	}
	return config
}

// OpenStore 打开 SQLite 并初始化 schema，首次启动会写入默认 settings。
func OpenStore(ctx context.Context, runtimeConfig RuntimeConfig) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(runtimeConfig.SQLitePath), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}

	db, err := sql.Open("sqlite", runtimeConfig.SQLitePath)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	db.SetMaxOpenConns(1)

	store := &Store{db: db, dbPath: runtimeConfig.SQLitePath}
	if err := store.configureSQLite(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.initSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.initDefaultSettings(ctx, runtimeConfig); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close 关闭 SQLite 连接，服务退出时调用以释放 WAL 相关文件句柄。
func (s *Store) Close() error {
	return s.db.Close()
}

// DBFileSize 返回 SQLite 主文件大小，后台摘要用它提示日志增长情况。
func (s *Store) DBFileSize() int64 {
	info, err := os.Stat(s.dbPath)
	if err != nil {
		return 0
	}
	return info.Size()
}

// configureSQLite 设置 WAL、busy_timeout 和 NORMAL synchronous，降低日志写入对读取的阻塞。
func (s *Store) configureSQLite(ctx context.Context) error {
	statements := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("设置 SQLite 参数失败: %w", err)
		}
	}
	return nil
}

// initSchema 创建所有业务表和索引，SQLite 外键约束按设计不启用。
func (s *Store) initSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value_json TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS upstreams (id TEXT PRIMARY KEY, name TEXT NOT NULL, base_url TEXT NOT NULL, api_key TEXT NOT NULL, auth_type TEXT NOT NULL, auth_header_name TEXT, auth_header_value_template TEXT, enabled INTEGER NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS api_key_groups (id TEXT PRIMARY KEY, name TEXT NOT NULL, api_key TEXT NOT NULL, default_upstream_id TEXT NOT NULL, enabled INTEGER NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS model_mappings (id TEXT PRIMARY KEY, group_id TEXT NOT NULL, client_model TEXT NOT NULL, upstream_model TEXT NOT NULL, upstream_id TEXT, enabled INTEGER NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(group_id, client_model))`,
		`CREATE TABLE IF NOT EXISTS request_logs (id TEXT PRIMARY KEY, started_at TEXT NOT NULL, completed_at TEXT NOT NULL, duration_ms INTEGER NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL, target_url TEXT NOT NULL, status_code INTEGER NOT NULL, is_stream INTEGER NOT NULL, api_key_group_id TEXT, upstream_id TEXT, upstream_host TEXT, client_model TEXT, upstream_model TEXT, claude_session_id TEXT, claude_agent_id TEXT, claude_parent_agent_id TEXT, request_bytes INTEGER NOT NULL, response_bytes INTEGER NOT NULL, request_body_truncated INTEGER NOT NULL, response_body_truncated INTEGER NOT NULL, log_mode TEXT NOT NULL, error TEXT NOT NULL, summary_json TEXT NOT NULL, request_headers_json TEXT, request_body BLOB, response_headers_json TEXT, response_body BLOB, response_body_mode TEXT)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_started_at ON request_logs(started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_status_code ON request_logs(status_code)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_client_model ON request_logs(client_model)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_upstream_model ON request_logs(upstream_model)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_upstream_id ON request_logs(upstream_id)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_group_id ON request_logs(api_key_group_id)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_session_id ON request_logs(claude_session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_agent_id ON request_logs(claude_agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_path ON request_logs(path)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("初始化 schema 失败: %w", err)
		}
	}
	return nil
}

// initDefaultSettings 在首次启动时写入默认配置，ADMIN_KEY 只在 admin_key_hash 缺失时生效。
func (s *Store) initDefaultSettings(ctx context.Context, runtimeConfig RuntimeConfig) error {
	settings := DefaultSettings(runtimeConfig.ListenAddress)
	adminKey := DefaultAdminKey
	if runtimeConfig.AdminKey != "" {
		adminKey = runtimeConfig.AdminKey
	}
	hash, err := HashSecret(adminKey)
	if err != nil {
		return err
	}
	settings.AdminKeyHash = hash

	defaults := map[string]any{
		"admin_key_hash":               settings.AdminKeyHash,
		"listen_address":               settings.ListenAddress,
		"log_mode":                     settings.LogMode,
		"log_retention_days":           settings.LogRetentionDays,
		"log_max_rows":                 settings.LogMaxRows,
		"log_cleanup_interval_minutes": settings.LogCleanupIntervalMinutes,
		"max_request_body_bytes":       settings.MaxRequestBodyBytes,
		"max_response_body_bytes":      settings.MaxResponseBodyBytes,
		"log_queue_size":               settings.LogQueueSize,
		"rewrite_model_in_response":    settings.RewriteModelInResponse,
	}
	for key, value := range defaults {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO settings (key, value_json, updated_at) VALUES (?, ?, ?)`, key, string(encoded), nowString()); err != nil {
			return fmt.Errorf("写入默认配置失败: %w", err)
		}
	}
	return nil
}

// DefaultSettings 返回全局配置默认值，这些值偏保守以优先保护转发链路稳定。
func DefaultSettings(listenAddress string) Settings {
	if listenAddress == "" {
		listenAddress = DefaultListenAddress
	}
	return Settings{
		ListenAddress:             listenAddress,
		LogMode:                   LogModeMetadata,
		LogRetentionDays:          14,
		LogMaxRows:                10000,
		LogCleanupIntervalMinutes: 60,
		MaxRequestBodyBytes:       128 * 1024,
		MaxResponseBodyBytes:      256 * 1024,
		LogQueueSize:              1024,
		RewriteModelInResponse:    false,
	}
}

// LoadSnapshot 从 SQLite 构建完整内存快照，管理写入成功后会重新调用它刷新请求路径视图。
func (s *Store) LoadSnapshot(ctx context.Context) (ConfigSnapshot, error) {
	settings, err := s.LoadSettings(ctx)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	upstreams, err := s.ListUpstreams(ctx)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	groups, err := s.ListGroups(ctx)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	mappings, err := s.ListMappings(ctx, "")
	if err != nil {
		return ConfigSnapshot{}, err
	}

	snapshot := ConfigSnapshot{
		Settings:        settings,
		Upstreams:       map[string]Upstream{},
		Groups:          map[string]APIKeyGroup{},
		GroupsByAPIKey:  map[string]APIKeyGroup{},
		MappingsByGroup: map[string]map[string]ModelMapping{},
	}
	for _, upstream := range upstreams {
		snapshot.Upstreams[upstream.ID] = upstream
	}
	for _, group := range groups {
		snapshot.Groups[group.ID] = group
		if group.Enabled {
			snapshot.GroupsByAPIKey[group.APIKey] = group
		}
	}
	for _, mapping := range mappings {
		if _, ok := snapshot.MappingsByGroup[mapping.GroupID]; !ok {
			snapshot.MappingsByGroup[mapping.GroupID] = map[string]ModelMapping{}
		}
		if mapping.Enabled {
			snapshot.MappingsByGroup[mapping.GroupID][mapping.ClientModel] = mapping
		}
	}
	return snapshot, nil
}

// LoadSettings 读取 settings 表并补齐缺失默认值，便于未来增加配置项时平滑启动。
func (s *Store) LoadSettings(ctx context.Context) (Settings, error) {
	settings := DefaultSettings("")
	rows, err := s.db.QueryContext(ctx, `SELECT key, value_json FROM settings`)
	if err != nil {
		return settings, err
	}
	defer rows.Close()

	for rows.Next() {
		var key, valueJSON string
		if err := rows.Scan(&key, &valueJSON); err != nil {
			return settings, err
		}
		switch key {
		case "admin_key_hash":
			_ = json.Unmarshal([]byte(valueJSON), &settings.AdminKeyHash)
		case "listen_address":
			_ = json.Unmarshal([]byte(valueJSON), &settings.ListenAddress)
		case "log_mode":
			_ = json.Unmarshal([]byte(valueJSON), &settings.LogMode)
		case "log_retention_days":
			_ = json.Unmarshal([]byte(valueJSON), &settings.LogRetentionDays)
		case "log_max_rows":
			_ = json.Unmarshal([]byte(valueJSON), &settings.LogMaxRows)
		case "log_cleanup_interval_minutes":
			_ = json.Unmarshal([]byte(valueJSON), &settings.LogCleanupIntervalMinutes)
		case "max_request_body_bytes":
			_ = json.Unmarshal([]byte(valueJSON), &settings.MaxRequestBodyBytes)
		case "max_response_body_bytes":
			_ = json.Unmarshal([]byte(valueJSON), &settings.MaxResponseBodyBytes)
		case "log_queue_size":
			_ = json.Unmarshal([]byte(valueJSON), &settings.LogQueueSize)
		case "rewrite_model_in_response":
			_ = json.Unmarshal([]byte(valueJSON), &settings.RewriteModelInResponse)
		}
	}
	if settings.LogMode == "" {
		settings.LogMode = LogModeMetadata
	}
	if settings.ListenAddress == "" {
		settings.ListenAddress = DefaultListenAddress
	}
	return settings, rows.Err()
}

// SaveLoggingSettings 更新日志相关 settings，限制字段范围是为了避免后台误配置拖垮代理路径。
func (s *Store) SaveLoggingSettings(ctx context.Context, patch Settings) error {
	if !validLogMode(patch.LogMode) {
		return errors.New("log_mode 必须是 off、metadata 或 full")
	}
	if patch.LogRetentionDays < 0 || patch.LogMaxRows < 0 || patch.LogCleanupIntervalMinutes < 1 || patch.MaxRequestBodyBytes < 0 || patch.MaxResponseBodyBytes < 0 || patch.LogQueueSize < 1 {
		return errors.New("日志设置不能为无效负数，清理间隔和队列大小必须大于 0")
	}
	updates := map[string]any{
		"log_mode":                     patch.LogMode,
		"log_retention_days":           patch.LogRetentionDays,
		"log_max_rows":                 patch.LogMaxRows,
		"log_cleanup_interval_minutes": patch.LogCleanupIntervalMinutes,
		"max_request_body_bytes":       patch.MaxRequestBodyBytes,
		"max_response_body_bytes":      patch.MaxResponseBodyBytes,
		"log_queue_size":               patch.LogQueueSize,
		"rewrite_model_in_response":    patch.RewriteModelInResponse,
	}
	return s.saveSettings(ctx, updates)
}

// SaveAdminKeyHash 保存新的后台密钥 hash，后台永远不回显旧密钥以减少泄露面。
func (s *Store) SaveAdminKeyHash(ctx context.Context, hash string) error {
	return s.saveSettings(ctx, map[string]any{"admin_key_hash": hash})
}

// saveSettings 在一个事务中写入多个 settings 项，保证后台保存后的快照一致。
func (s *Store) saveSettings(ctx context.Context, updates map[string]any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackUnlessCommitted(tx)
	for key, value := range updates {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value_json, updated_at) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`, key, string(encoded), nowString()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListUpstreams 返回所有上游，管理后台需要同时展示启用和停用项。
func (s *Store) ListUpstreams(ctx context.Context) ([]Upstream, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, base_url, api_key, auth_type, COALESCE(auth_header_name, ''), COALESCE(auth_header_value_template, ''), enabled, created_at, updated_at FROM upstreams ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var upstreams []Upstream
	for rows.Next() {
		upstream, err := scanUpstream(rows)
		if err != nil {
			return nil, err
		}
		upstreams = append(upstreams, upstream)
	}
	return upstreams, rows.Err()
}

// SaveUpstream 创建或更新上游，更新时空 API Key 表示保留旧值以避免页面必须回显密钥。
func (s *Store) SaveUpstream(ctx context.Context, upstream Upstream) (Upstream, error) {
	if err := validateUpstream(upstream); err != nil {
		return upstream, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return upstream, err
	}
	defer rollbackUnlessCommitted(tx)
	if upstream.ID == "" {
		upstream.ID = newID("up")
		upstream.CreatedAt = now
	} else {
		var oldKey string
		if upstream.APIKey == "" {
			_ = tx.QueryRowContext(ctx, `SELECT api_key FROM upstreams WHERE id = ?`, upstream.ID).Scan(&oldKey)
			upstream.APIKey = oldKey
		}
		var created string
		if err := tx.QueryRowContext(ctx, `SELECT created_at FROM upstreams WHERE id = ?`, upstream.ID).Scan(&created); err == nil {
			upstream.CreatedAt = parseTime(created)
		}
	}
	upstream.UpdatedAt = now
	if upstream.AuthType == "" {
		upstream.AuthType = AuthTypeXAPIKey
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO upstreams (id, name, base_url, api_key, auth_type, auth_header_name, auth_header_value_template, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name, base_url = excluded.base_url, api_key = excluded.api_key, auth_type = excluded.auth_type, auth_header_name = excluded.auth_header_name, auth_header_value_template = excluded.auth_header_value_template, enabled = excluded.enabled, updated_at = excluded.updated_at`, upstream.ID, upstream.Name, upstream.BaseURL, upstream.APIKey, upstream.AuthType, nullableString(upstream.AuthHeaderName), nullableString(upstream.AuthHeaderValueTemplate), boolInt(upstream.Enabled), formatTime(upstream.CreatedAt), formatTime(upstream.UpdatedAt))
	if err != nil {
		return upstream, err
	}
	return upstream, tx.Commit()
}

// DeleteUpstream 删除未被引用的上游，被 group 或 mapping 引用时拒绝删除以保持逻辑外键完整。
func (s *Store) DeleteUpstream(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackUnlessCommitted(tx)
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_key_groups WHERE default_upstream_id = ?`, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("上游正在被 API Key 组引用，不能删除")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_mappings WHERE upstream_id = ?`, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("上游正在被模型映射引用，不能删除")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM upstreams WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListGroups 返回所有 API Key 组，附带明文 key 是设计要求下的单机可视化权衡。
func (s *Store) ListGroups(ctx context.Context) ([]APIKeyGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, api_key, default_upstream_id, enabled, created_at, updated_at FROM api_key_groups ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []APIKeyGroup
	for rows.Next() {
		var group APIKeyGroup
		var enabled int
		var createdAt, updatedAt string
		if err := rows.Scan(&group.ID, &group.Name, &group.APIKey, &group.DefaultUpstreamID, &enabled, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		group.Enabled = enabled == 1
		group.CreatedAt = parseTime(createdAt)
		group.UpdatedAt = parseTime(updatedAt)
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// SaveGroup 创建或更新 API Key 组，并校验默认上游存在，避免请求期才暴露配置错误。
func (s *Store) SaveGroup(ctx context.Context, group APIKeyGroup) (APIKeyGroup, error) {
	if strings.TrimSpace(group.Name) == "" || strings.TrimSpace(group.APIKey) == "" || strings.TrimSpace(group.DefaultUpstreamID) == "" {
		return group, errors.New("group 名称、API Key 和默认上游不能为空")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return group, err
	}
	defer rollbackUnlessCommitted(tx)
	if !rowExists(ctx, tx, "upstreams", group.DefaultUpstreamID) {
		return group, errors.New("默认上游不存在")
	}
	now := time.Now().UTC()
	if group.ID == "" {
		group.ID = newID("grp")
		group.CreatedAt = now
	} else {
		var created string
		if err := tx.QueryRowContext(ctx, `SELECT created_at FROM api_key_groups WHERE id = ?`, group.ID).Scan(&created); err == nil {
			group.CreatedAt = parseTime(created)
		}
	}
	group.UpdatedAt = now
	_, err = tx.ExecContext(ctx, `INSERT INTO api_key_groups (id, name, api_key, default_upstream_id, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name, api_key = excluded.api_key, default_upstream_id = excluded.default_upstream_id, enabled = excluded.enabled, updated_at = excluded.updated_at`, group.ID, group.Name, group.APIKey, group.DefaultUpstreamID, boolInt(group.Enabled), formatTime(group.CreatedAt), formatTime(group.UpdatedAt))
	if err != nil {
		return group, err
	}
	return group, tx.Commit()
}

// DeleteGroup 删除未被模型映射引用的 API Key 组，避免孤儿映射污染后台选择。
func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackUnlessCommitted(tx)
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_mappings WHERE group_id = ?`, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("API Key 组仍有模型映射，不能删除")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_groups WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListMappings 返回模型映射，groupID 为空时返回所有组以便构建配置快照。
func (s *Store) ListMappings(ctx context.Context, groupID string) ([]ModelMapping, error) {
	query := `SELECT id, group_id, client_model, upstream_model, COALESCE(upstream_id, ''), enabled, created_at, updated_at FROM model_mappings`
	args := []any{}
	if groupID != "" {
		query += ` WHERE group_id = ?`
		args = append(args, groupID)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var mappings []ModelMapping
	for rows.Next() {
		var mapping ModelMapping
		var enabled int
		var createdAt, updatedAt string
		if err := rows.Scan(&mapping.ID, &mapping.GroupID, &mapping.ClientModel, &mapping.UpstreamModel, &mapping.UpstreamID, &enabled, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		mapping.Enabled = enabled == 1
		mapping.CreatedAt = parseTime(createdAt)
		mapping.UpdatedAt = parseTime(updatedAt)
		mappings = append(mappings, mapping)
	}
	return mappings, rows.Err()
}

// SaveMapping 创建或更新模型映射，强制校验 group 和可选上游引用来模拟逻辑外键。
func (s *Store) SaveMapping(ctx context.Context, mapping ModelMapping) (ModelMapping, error) {
	if strings.TrimSpace(mapping.GroupID) == "" || strings.TrimSpace(mapping.ClientModel) == "" || strings.TrimSpace(mapping.UpstreamModel) == "" {
		return mapping, errors.New("group_id、client_model 和 upstream_model 不能为空")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapping, err
	}
	defer rollbackUnlessCommitted(tx)
	if !rowExists(ctx, tx, "api_key_groups", mapping.GroupID) {
		return mapping, errors.New("API Key 组不存在")
	}
	if mapping.UpstreamID != "" && !rowExists(ctx, tx, "upstreams", mapping.UpstreamID) {
		return mapping, errors.New("覆盖上游不存在")
	}
	now := time.Now().UTC()
	if mapping.ID == "" {
		mapping.ID = newID("map")
		mapping.CreatedAt = now
	} else {
		var created string
		if err := tx.QueryRowContext(ctx, `SELECT created_at FROM model_mappings WHERE id = ?`, mapping.ID).Scan(&created); err == nil {
			mapping.CreatedAt = parseTime(created)
		}
	}
	mapping.UpdatedAt = now
	_, err = tx.ExecContext(ctx, `INSERT INTO model_mappings (id, group_id, client_model, upstream_model, upstream_id, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET group_id = excluded.group_id, client_model = excluded.client_model, upstream_model = excluded.upstream_model, upstream_id = excluded.upstream_id, enabled = excluded.enabled, updated_at = excluded.updated_at`, mapping.ID, mapping.GroupID, mapping.ClientModel, mapping.UpstreamModel, nullableString(mapping.UpstreamID), boolInt(mapping.Enabled), formatTime(mapping.CreatedAt), formatTime(mapping.UpdatedAt))
	if err != nil {
		return mapping, err
	}
	return mapping, tx.Commit()
}

// DeleteMapping 删除单条模型映射，删除后对应 client_model 会立即从模型发现结果中消失。
func (s *Store) DeleteMapping(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackUnlessCommitted(tx)
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_mappings WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DashboardSummary 聚合后台首页需要的轻量状态，避免前端自行拼装多次请求。
type DashboardSummary struct {
	UpstreamCount   int    `json:"upstream_count"`
	MappingCount    int    `json:"mapping_count"`
	LogRowCount     int64  `json:"log_row_count"`
	LastRequestAt   string `json:"last_request_at,omitempty"`
	SQLiteFileSize  int64  `json:"sqlite_file_size"`
	DefaultAdminKey bool   `json:"default_admin_key"`
	CurrentLogMode  string `json:"current_log_mode"`
	SelectedGroupID string `json:"selected_group_id,omitempty"`
}

// DashboardData 是后台首页聚合 DTO，所有配置编辑区可由一次请求完成初始化。
type DashboardData struct {
	Summary       DashboardSummary `json:"summary"`
	Settings      Settings         `json:"settings"`
	Upstreams     []Upstream       `json:"upstreams"`
	Groups        []APIKeyGroup    `json:"groups"`
	Mappings      []ModelMapping   `json:"mappings"`
	MappingCounts map[string]int   `json:"mapping_counts"`
}

// Dashboard 返回管理首页初始数据，并用当前管理密钥 hash 判断默认密钥提醒状态。
func (s *Store) Dashboard(ctx context.Context) (DashboardData, error) {
	settings, err := s.LoadSettings(ctx)
	if err != nil {
		return DashboardData{}, err
	}
	upstreams, err := s.ListUpstreams(ctx)
	if err != nil {
		return DashboardData{}, err
	}
	groups, err := s.ListGroups(ctx)
	if err != nil {
		return DashboardData{}, err
	}
	mappings, err := s.ListMappings(ctx, "")
	if err != nil {
		return DashboardData{}, err
	}
	var logRows int64
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_logs`).Scan(&logRows)
	var lastRequest sql.NullString
	_ = s.db.QueryRowContext(ctx, `SELECT MAX(started_at) FROM request_logs`).Scan(&lastRequest)
	mappingCounts := map[string]int{}
	for _, mapping := range mappings {
		mappingCounts[mapping.GroupID]++
	}
	selectedGroupID := ""
	if len(groups) > 0 {
		selectedGroupID = groups[0].ID
	}
	defaultAdmin := VerifySecret(DefaultAdminKey, settings.AdminKeyHash)
	return DashboardData{
		Summary: DashboardSummary{
			UpstreamCount:   len(upstreams),
			MappingCount:    len(mappings),
			LogRowCount:     logRows,
			LastRequestAt:   lastRequest.String,
			SQLiteFileSize:  s.DBFileSize(),
			DefaultAdminKey: defaultAdmin,
			CurrentLogMode:  settings.LogMode,
			SelectedGroupID: selectedGroupID,
		},
		Settings:      settings,
		Upstreams:     upstreams,
		Groups:        groups,
		Mappings:      mappings,
		MappingCounts: mappingCounts,
	}, nil
}

// HashSecret 使用随机 salt 和 SHA-256 保存管理密钥，单机后台无需引入额外密码库依赖。
func HashSecret(secret string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest := sha256.Sum256(append(salt, []byte(secret)...))
	return "sha256$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + hex.EncodeToString(digest[:]), nil
}

// VerifySecret 校验明文密钥和保存的 hash，hash 格式异常时直接失败而不兜底默认密码。
func VerifySecret(secret string, storedHash string) bool {
	parts := strings.Split(storedHash, "$")
	if len(parts) != 3 || parts[0] != "sha256" {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	digest := sha256.Sum256(append(salt, []byte(secret)...))
	return hex.EncodeToString(digest[:]) == parts[2]
}

// validateUpstream 校验上游配置，base_url 必须可解析是为了避免请求期拼接出危险目标。
func validateUpstream(upstream Upstream) error {
	if strings.TrimSpace(upstream.Name) == "" || strings.TrimSpace(upstream.BaseURL) == "" {
		return errors.New("上游名称和 base_url 不能为空")
	}
	parsed, err := url.Parse(upstream.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("上游 base_url 必须是有效 URL")
	}
	if upstream.ID == "" && strings.TrimSpace(upstream.APIKey) == "" {
		return errors.New("新增上游必须提供 API Key")
	}
	if upstream.AuthType == "" {
		upstream.AuthType = AuthTypeXAPIKey
	}
	if upstream.AuthType != AuthTypeXAPIKey && upstream.AuthType != AuthTypeAuthorizationBearer && upstream.AuthType != AuthTypeCustomHeader {
		return errors.New("上游 auth_type 无效")
	}
	if upstream.AuthType == AuthTypeCustomHeader && strings.TrimSpace(upstream.AuthHeaderName) == "" {
		return errors.New("custom_header 认证必须提供 header name")
	}
	return nil
}

// scanUpstream 从 SQL 行读取 Upstream，集中处理 SQLite 整数布尔值转换。
func scanUpstream(rows *sql.Rows) (Upstream, error) {
	var upstream Upstream
	var enabled int
	var createdAt, updatedAt string
	if err := rows.Scan(&upstream.ID, &upstream.Name, &upstream.BaseURL, &upstream.APIKey, &upstream.AuthType, &upstream.AuthHeaderName, &upstream.AuthHeaderValueTemplate, &enabled, &createdAt, &updatedAt); err != nil {
		return upstream, err
	}
	upstream.Enabled = enabled == 1
	upstream.CreatedAt = parseTime(createdAt)
	upstream.UpdatedAt = parseTime(updatedAt)
	return upstream, nil
}

// newID 生成带业务前缀的随机 ID，便于后台排查不同实体类型。
func newID(prefix string) string {
	randomBytes := make([]byte, 12)
	_, _ = rand.Read(randomBytes)
	return prefix + "_" + hex.EncodeToString(randomBytes)
}

// nowString 返回 SQLite 中统一保存的 UTC 时间字符串。
func nowString() string {
	return formatTime(time.Now().UTC())
}

// formatTime 以 RFC3339Nano 保存时间，既可排序又能保留足够精度。
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime 解析数据库时间，失败时返回零值以避免后台渲染崩溃。
func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// boolInt 将 Go bool 转换为 SQLite INTEGER，保持 schema 简洁。
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// nullableString 将空字符串写为 NULL，符合 mapping upstream_id 为空时使用默认上游的语义。
func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// validLogMode 判断日志模式是否在设计规格的三档范围内。
func validLogMode(mode string) bool {
	return mode == LogModeOff || mode == LogModeMetadata || mode == LogModeFull
}

// rowExists 检查逻辑外键目标是否存在，queryer 允许配置事务内复用同一读写视图。
func rowExists(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, table string, id string) bool {
	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = ?", table)
	if err := queryer.QueryRowContext(ctx, query, id).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// getenvDefault 读取环境变量，未设置时返回默认值。
func getenvDefault(key string, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

// rollbackUnlessCommitted 统一忽略已提交事务的 rollback 错误，减少每个写路径重复代码。
func rollbackUnlessCommitted(tx *sql.Tx) {
	_ = tx.Rollback()
}

// atoiDefault 将查询参数转为整数，非法值使用默认值而不是让列表接口失败。
func atoiDefault(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
