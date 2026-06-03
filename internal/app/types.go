package app

import "time"

const (
	// DefaultSQLitePath 是单机部署的默认数据库位置，放在 data 下便于整体备份。
	DefaultSQLitePath = "./data/app.sqlite"
	// DefaultListenAddress 是未配置 LISTEN_ADDR 时使用的监听地址。
	DefaultListenAddress = ":8080"
	// DefaultAdminKey 是首次启动未传 ADMIN_KEY 时的后台默认密钥。
	DefaultAdminKey = "defaultpassword"

	// LogModeOff 表示完全不写 request_logs，避免日志链路影响高压转发。
	LogModeOff = "off"
	// LogModeMetadata 表示只写请求元数据，不保存 headers 和 payload。
	LogModeMetadata = "metadata"
	// LogModeFull 表示写入元数据、headers 和受限长度的 payload。
	LogModeFull = "full"

	// AuthTypeXAPIKey 表示上游认证使用 x-api-key 请求头。
	AuthTypeXAPIKey = "x_api_key"
	// AuthTypeAuthorizationBearer 表示上游认证使用 Authorization: Bearer。
	AuthTypeAuthorizationBearer = "authorization_bearer"
	// AuthTypeCustomHeader 表示上游认证使用用户定义的请求头和值模板。
	AuthTypeCustomHeader = "custom_header"
)

// RuntimeConfig 描述进程启动时从环境变量读取的配置，避免把部署入口耦合到数据库实现。
type RuntimeConfig struct {
	SQLitePath    string
	ListenAddress string
	AdminKey      string
}

// Settings 描述全局运行配置，网关请求和日志后台都从内存快照读取它。
type Settings struct {
	AdminKeyHash              string `json:"-"`
	ListenAddress             string `json:"listen_address"`
	LogMode                   string `json:"log_mode"`
	LogRetentionDays          int    `json:"log_retention_days"`
	LogMaxRows                int    `json:"log_max_rows"`
	LogCleanupIntervalMinutes int    `json:"log_cleanup_interval_minutes"`
	MaxRequestBodyBytes       int64  `json:"max_request_body_bytes"`
	MaxResponseBodyBytes      int64  `json:"max_response_body_bytes"`
	LogQueueSize              int    `json:"log_queue_size"`
	RewriteModelInResponse    bool   `json:"rewrite_model_in_response"`
}

// Upstream 描述一个真实 Anthropic Messages 上游，认证策略必须独立保存以支持多供应商代理。
type Upstream struct {
	ID                      string    `json:"id"`
	Name                    string    `json:"name"`
	BaseURL                 string    `json:"base_url"`
	APIKey                  string    `json:"api_key,omitempty"`
	AuthType                string    `json:"auth_type"`
	AuthHeaderName          string    `json:"auth_header_name,omitempty"`
	AuthHeaderValueTemplate string    `json:"auth_header_value_template,omitempty"`
	Enabled                 bool      `json:"enabled"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// APIKeyGroup 绑定网关 API Key、默认上游和模型映射集合，确保不同客户端可使用不同模型策略。
type APIKeyGroup struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	APIKey            string    `json:"api_key"`
	DefaultUpstreamID string    `json:"default_upstream_id"`
	Enabled           bool      `json:"enabled"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// ModelMapping 描述 Claude Code 请求侧模型名到上游真实模型名的显式映射。
type ModelMapping struct {
	ID            string    `json:"id"`
	GroupID       string    `json:"group_id"`
	ClientModel   string    `json:"client_model"`
	UpstreamModel string    `json:"upstream_model"`
	UpstreamID    string    `json:"upstream_id,omitempty"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ConfigSnapshot 是请求路径只读的配置快照，避免每个代理请求都访问 SQLite。
type ConfigSnapshot struct {
	Settings        Settings
	Upstreams       map[string]Upstream
	Groups          map[string]APIKeyGroup
	GroupsByAPIKey  map[string]APIKeyGroup
	MappingsByGroup map[string]map[string]ModelMapping
}

// RequestLog 是异步日志队列中的完整记录，字段与 request_logs 表保持一一对应。
type RequestLog struct {
	ID                    string
	StartedAt             time.Time
	CompletedAt           time.Time
	DurationMS            int64
	Method                string
	Path                  string
	TargetURL             string
	StatusCode            int
	IsStream              bool
	APIKeyGroupID         string
	UpstreamID            string
	UpstreamHost          string
	ClientModel           string
	UpstreamModel         string
	ClaudeSessionID       string
	ClaudeAgentID         string
	ClaudeParentAgentID   string
	RequestBytes          int64
	ResponseBytes         int64
	RequestBodyTruncated  bool
	ResponseBodyTruncated bool
	LogMode               string
	Error                 string
	SummaryJSON           string
	RequestHeadersJSON    *string
	RequestBody           []byte
	ResponseHeadersJSON   *string
	ResponseBody          []byte
	ResponseBodyMode      *string
}

// LogFilter 描述日志列表的查询条件，后端统一做分页限制以保护 SQLite。
type LogFilter struct {
	Page          int
	PageSize      int
	Search        string
	StartedAfter  string
	StartedBefore string
	StatusCode    string
	Path          string
	ClientModel   string
	UpstreamModel string
	UpstreamID    string
	GroupID       string
	SessionID     string
	AgentID       string
	IsStream      string
	Truncated     string
}

// LogListItem 是日志列表行 DTO，只返回排查所需摘要字段以减少页面加载成本。
type LogListItem struct {
	ID                string `json:"id"`
	StartedAt         string `json:"started_at"`
	DurationMS        int64  `json:"duration_ms"`
	Method            string `json:"method"`
	Path              string `json:"path"`
	StatusCode        int    `json:"status_code"`
	IsStream          bool   `json:"is_stream"`
	APIKeyGroupID     string `json:"api_key_group_id,omitempty"`
	APIKeyGroupName   string `json:"api_key_group_name,omitempty"`
	UpstreamID        string `json:"upstream_id,omitempty"`
	UpstreamName      string `json:"upstream_name,omitempty"`
	ClientModel       string `json:"client_model,omitempty"`
	UpstreamModel     string `json:"upstream_model,omitempty"`
	ClaudeSessionID   string `json:"claude_session_id,omitempty"`
	RequestTruncated  bool   `json:"request_truncated"`
	ResponseTruncated bool   `json:"response_truncated"`
	ResponseBytes     int64  `json:"response_bytes"`
	RequestBytes      int64  `json:"request_bytes"`
}

// PagedLogs 是日志列表接口响应 DTO，显式返回 total 便于前端分页。
type PagedLogs struct {
	Items    []LogListItem `json:"items"`
	Total    int64         `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

// LogDetail 是日志详情 DTO，full 模式下包含 headers 和截断后的 payload。
type LogDetail struct {
	LogListItem
	TargetURL             string  `json:"target_url"`
	UpstreamHost          string  `json:"upstream_host,omitempty"`
	ClaudeAgentID         string  `json:"claude_agent_id,omitempty"`
	ClaudeParentAgentID   string  `json:"claude_parent_agent_id,omitempty"`
	CompletedAt           string  `json:"completed_at"`
	RequestBodyTruncated  bool    `json:"request_body_truncated"`
	ResponseBodyTruncated bool    `json:"response_body_truncated"`
	LogMode               string  `json:"log_mode"`
	Error                 string  `json:"error,omitempty"`
	SummaryJSON           string  `json:"summary_json"`
	RequestHeadersJSON    *string `json:"request_headers_json,omitempty"`
	RequestBody           *string `json:"request_body,omitempty"`
	ResponseHeadersJSON   *string `json:"response_headers_json,omitempty"`
	ResponseBody          *string `json:"response_body,omitempty"`
	ResponseBodyMode      *string `json:"response_body_mode,omitempty"`
}
