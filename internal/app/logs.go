package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// LogWriter 异步写入 request_logs，队列满时丢弃日志以保护代理响应。
type LogWriter struct {
	store *Store
	queue chan RequestLog
}

// NewLogWriter 创建日志 writer，queueSize 来自 settings 以便单机部署按内存调整。
func NewLogWriter(store *Store, queueSize int) *LogWriter {
	if queueSize < 1 {
		queueSize = 1
	}
	return &LogWriter{store: store, queue: make(chan RequestLog, queueSize)}
}

// Start 启动后台批量写入循环，ctx 结束后会尝试 flush 已经入队的日志。
func (w *LogWriter) Start(ctx context.Context) {
	go w.run(ctx)
}

// Enqueue 将日志放入队列，full/off 的判定在这里做可以让代理路径保持简单。
func (w *LogWriter) Enqueue(record RequestLog) {
	if record.LogMode == LogModeOff {
		return
	}
	select {
	case w.queue <- record:
	default:
	}
}

// run 批量写入日志，减少 SQLite 写锁频率。
func (w *LogWriter) run(ctx context.Context) {
	batch := make([]RequestLog, 0, 32)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case record := <-w.queue:
					batch = append(batch, record)
				default:
					w.flush(batch)
					return
				}
			}
		case record := <-w.queue:
			batch = append(batch, record)
			if len(batch) >= 32 {
				w.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				w.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

// flush 将一批日志写入 SQLite，失败只写内部日志，不能反向影响客户端请求。
// SSE 收集在此异步执行，确保不阻塞代理转发路径。
func (w *LogWriter) flush(batch []RequestLog) {
	if len(batch) == 0 {
		return
	}
	for i := range batch {
		if batch[i].IsStream && len(batch[i].ResponseBody) > 0 && (batch[i].SummaryJSON == "" || batch[i].SummaryJSON == "{}") {
			batch[i].SummaryJSON = collectSSEMessage(batch[i].ResponseBody)
		}
	}
	if err := w.store.InsertLogs(context.Background(), batch); err != nil {
		log.Printf("写入请求日志失败: %v", err)
	}
}

// InsertLogs 直接追加日志记录，不使用事务以避免日志链路占用配置写入所需的一致性成本。
func (s *Store) InsertLogs(ctx context.Context, records []RequestLog) error {
	statement, err := s.db.PrepareContext(ctx, `INSERT INTO request_logs (id, started_at, completed_at, duration_ms, method, path, target_url, status_code, is_stream, api_key_group_id, upstream_id, upstream_host, client_model, upstream_model, claude_session_id, claude_agent_id, claude_parent_agent_id, request_bytes, response_bytes, request_body_truncated, response_body_truncated, log_mode, error, summary_json, request_headers_json, request_body, response_headers_json, response_body, response_body_mode) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, record := range records {
		if record.CompletedAt.IsZero() {
			record.CompletedAt = time.Now().UTC()
		}
		if record.SummaryJSON == "" {
			record.SummaryJSON = "{}"
		}
		_, err := statement.ExecContext(ctx,
			record.ID,
			formatTime(record.StartedAt),
			formatTime(record.CompletedAt),
			record.DurationMS,
			record.Method,
			record.Path,
			record.TargetURL,
			record.StatusCode,
			boolInt(record.IsStream),
			nullableString(record.APIKeyGroupID),
			nullableString(record.UpstreamID),
			nullableString(record.UpstreamHost),
			nullableString(record.ClientModel),
			nullableString(record.UpstreamModel),
			nullableString(record.ClaudeSessionID),
			nullableString(record.ClaudeAgentID),
			nullableString(record.ClaudeParentAgentID),
			record.RequestBytes,
			record.ResponseBytes,
			boolInt(record.RequestBodyTruncated),
			boolInt(record.ResponseBodyTruncated),
			record.LogMode,
			record.Error,
			record.SummaryJSON,
			nullableStringPointer(record.RequestHeadersJSON),
			nullableBytes(record.RequestBody),
			nullableStringPointer(record.ResponseHeadersJSON),
			nullableBytes(record.ResponseBody),
			nullableStringPointer(record.ResponseBodyMode),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// ListLogs 按筛选条件分页查询日志，所有过滤项都使用参数绑定避免 SQL 注入。
func (s *Store) ListLogs(ctx context.Context, filter LogFilter) (PagedLogs, error) {
	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.PageSize < 1 {
		filter.PageSize = 25
	}
	if filter.PageSize > 100 {
		filter.PageSize = 100
	}
	where, args := buildLogWhere(filter)
	countQuery := `SELECT COUNT(*) FROM request_logs l` + where
	var total int64
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return PagedLogs{}, err
	}
	query := `SELECT l.id, l.started_at, l.duration_ms, l.method, l.path, l.status_code, l.is_stream, COALESCE(l.api_key_group_id, ''), COALESCE(g.name, ''), COALESCE(l.upstream_id, ''), COALESCE(u.name, ''), COALESCE(l.client_model, ''), COALESCE(l.upstream_model, ''), COALESCE(l.claude_session_id, ''), l.request_body_truncated, l.response_body_truncated, l.response_bytes, l.request_bytes FROM request_logs l LEFT JOIN api_key_groups g ON l.api_key_group_id = g.id LEFT JOIN upstreams u ON l.upstream_id = u.id` + where + ` ORDER BY l.started_at DESC LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), filter.PageSize, (filter.Page-1)*filter.PageSize)
	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return PagedLogs{}, err
	}
	defer rows.Close()
	items := []LogListItem{}
	for rows.Next() {
		var item LogListItem
		var isStream, requestTruncated, responseTruncated int
		if err := rows.Scan(&item.ID, &item.StartedAt, &item.DurationMS, &item.Method, &item.Path, &item.StatusCode, &isStream, &item.APIKeyGroupID, &item.APIKeyGroupName, &item.UpstreamID, &item.UpstreamName, &item.ClientModel, &item.UpstreamModel, &item.ClaudeSessionID, &requestTruncated, &responseTruncated, &item.ResponseBytes, &item.RequestBytes); err != nil {
			return PagedLogs{}, err
		}
		item.IsStream = isStream == 1
		item.RequestTruncated = requestTruncated == 1
		item.ResponseTruncated = responseTruncated == 1
		items = append(items, item)
	}
	return PagedLogs{Items: items, Total: total, Page: filter.Page, PageSize: filter.PageSize}, rows.Err()
}

// GetLogDetail 按 ID 读取单条日志详情，payload 以 base64 字符串返回以保留二进制安全性。
func (s *Store) GetLogDetail(ctx context.Context, id string) (LogDetail, error) {
	query := `SELECT l.id, l.started_at, l.completed_at, l.duration_ms, l.method, l.path, l.target_url, l.status_code, l.is_stream, COALESCE(l.api_key_group_id, ''), COALESCE(g.name, ''), COALESCE(l.upstream_id, ''), COALESCE(u.name, ''), COALESCE(l.upstream_host, ''), COALESCE(l.client_model, ''), COALESCE(l.upstream_model, ''), COALESCE(l.claude_session_id, ''), COALESCE(l.claude_agent_id, ''), COALESCE(l.claude_parent_agent_id, ''), l.request_bytes, l.response_bytes, l.request_body_truncated, l.response_body_truncated, l.log_mode, l.error, l.summary_json, l.request_headers_json, l.request_body, l.response_headers_json, l.response_body, l.response_body_mode FROM request_logs l LEFT JOIN api_key_groups g ON l.api_key_group_id = g.id LEFT JOIN upstreams u ON l.upstream_id = u.id WHERE l.id = ?`
	var detail LogDetail
	var isStream, requestTruncated, responseTruncated int
	var completedAt string
	var requestHeaders, responseHeaders, responseBodyMode sql.NullString
	var requestBody, responseBody []byte
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&detail.ID,
		&detail.StartedAt,
		&completedAt,
		&detail.DurationMS,
		&detail.Method,
		&detail.Path,
		&detail.TargetURL,
		&detail.StatusCode,
		&isStream,
		&detail.APIKeyGroupID,
		&detail.APIKeyGroupName,
		&detail.UpstreamID,
		&detail.UpstreamName,
		&detail.UpstreamHost,
		&detail.ClientModel,
		&detail.UpstreamModel,
		&detail.ClaudeSessionID,
		&detail.ClaudeAgentID,
		&detail.ClaudeParentAgentID,
		&detail.RequestBytes,
		&detail.ResponseBytes,
		&requestTruncated,
		&responseTruncated,
		&detail.LogMode,
		&detail.Error,
		&detail.SummaryJSON,
		&requestHeaders,
		&requestBody,
		&responseHeaders,
		&responseBody,
		&responseBodyMode,
	)
	if err != nil {
		return LogDetail{}, err
	}
	detail.CompletedAt = completedAt
	detail.IsStream = isStream == 1
	detail.RequestTruncated = requestTruncated == 1
	detail.ResponseTruncated = responseTruncated == 1
	detail.RequestBodyTruncated = detail.RequestTruncated
	detail.ResponseBodyTruncated = detail.ResponseTruncated
	if requestHeaders.Valid {
		detail.RequestHeadersJSON = &requestHeaders.String
	}
	if responseHeaders.Valid {
		detail.ResponseHeadersJSON = &responseHeaders.String
	}
	if len(requestBody) > 0 {
		encoded := base64.StdEncoding.EncodeToString(requestBody)
		detail.RequestBody = &encoded
	}
	if len(responseBody) > 0 {
		encoded := base64.StdEncoding.EncodeToString(responseBody)
		detail.ResponseBody = &encoded
	}
	if responseBodyMode.Valid {
		detail.ResponseBodyMode = &responseBodyMode.String
	}
	return detail, nil
}

// CleanupLogs 应用保留天数和最大行数两种策略，日志清理不使用事务以保持低优先级。
func (s *Store) CleanupLogs(ctx context.Context, settings Settings) error {
	if settings.LogRetentionDays > 0 {
		cutoff := time.Now().UTC().Add(-time.Duration(settings.LogRetentionDays) * 24 * time.Hour)
		if _, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE started_at < ?`, formatTime(cutoff)); err != nil {
			return err
		}
	}
	if settings.LogMaxRows > 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE id NOT IN (SELECT id FROM request_logs ORDER BY started_at DESC LIMIT ?)`, settings.LogMaxRows)
		if err != nil {
			return err
		}
	}
	return nil
}

// buildLogWhere 生成日志查询 WHERE 子句，返回的片段同时适配带别名 l 的列表查询。
func buildLogWhere(filter LogFilter) (string, []any) {
	conditions := []string{}
	args := []any{}
	addLike := func(column string, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		conditions = append(conditions, column+" LIKE ?")
		args = append(args, "%"+strings.TrimSpace(value)+"%")
	}
	if filter.Search != "" {
		conditions = append(conditions, `(l.path LIKE ? OR l.client_model LIKE ? OR l.upstream_model LIKE ? OR l.claude_session_id LIKE ?)`)
		like := "%" + strings.TrimSpace(filter.Search) + "%"
		args = append(args, like, like, like, like)
	}
	addLike("l.path", filter.Path)
	addLike("l.client_model", filter.ClientModel)
	addLike("l.upstream_model", filter.UpstreamModel)
	addLike("l.upstream_id", filter.UpstreamID)
	addLike("l.api_key_group_id", filter.GroupID)
	addLike("l.claude_session_id", filter.SessionID)
	addLike("l.claude_agent_id", filter.AgentID)
	if filter.StartedAfter != "" {
		conditions = append(conditions, "l.started_at >= ?")
		args = append(args, filter.StartedAfter)
	}
	if filter.StartedBefore != "" {
		conditions = append(conditions, "l.started_at <= ?")
		args = append(args, filter.StartedBefore)
	}
	if filter.StatusCode != "" {
		if status, err := strconv.Atoi(filter.StatusCode); err == nil {
			conditions = append(conditions, "l.status_code = ?")
			args = append(args, status)
		}
	}
	if filter.IsStream == "true" || filter.IsStream == "false" {
		conditions = append(conditions, "l.is_stream = ?")
		args = append(args, boolStringInt(filter.IsStream))
	}
	if filter.Truncated == "true" {
		conditions = append(conditions, "(l.request_body_truncated = 1 OR l.response_body_truncated = 1)")
	} else if filter.Truncated == "false" {
		conditions = append(conditions, "l.request_body_truncated = 0 AND l.response_body_truncated = 0")
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// nullableStringPointer 将 nil 指针写入 NULL，非 nil 写入其字符串内容。
func nullableStringPointer(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// nullableBytes 将空 payload 写为 NULL，使 metadata 模式和空 body 可区分。
func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

// boolStringInt 将查询参数布尔值转换为 SQLite INTEGER。
func boolStringInt(value string) int {
	if value == "true" {
		return 1
	}
	return 0
}

// noRowsError 判断日志详情不存在，供 HTTP 层返回 404。
func noRowsError(err error) bool {
	return err == sql.ErrNoRows
}

// logQueryError 包装日志查询错误，便于调用方保留原始错误链。
func logQueryError(action string, err error) error {
	return fmt.Errorf("%s 日志失败: %w", action, err)
}
