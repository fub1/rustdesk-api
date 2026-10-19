package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
)

// 不需要记录日志的路径（登录类 + POST 查询类）
var skipOpLogPaths = map[string]bool{
	"/api/admin/login":         true,
	"/api/admin/logout":        true,
	"/api/admin/captcha":       true,
	"/api/admin/login-options": true,
	"/api/admin/oidc/auth":     true,
	// 以下 POST 接口实际是查询操作，不修改数据
	"/api/admin/peer/simpleData": true,
	"/api/admin/user/myOauth":    true,
	"/api/admin/user/groupUsers": true,
}

// OperationLog 操作日志中间件，记录所有 POST 写操作
// 必须在 BackendUserAuth 之后使用，以便获取当前用户信息
func OperationLog() gin.HandlerFunc {
	writer := service.AllService.OperationLogService
	return operationLog(writer.Create)
}

func operationLog(write func(*model.OperationLog) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 只记录 POST 请求
		if c.Request.Method != "POST" {
			c.Next()
			return
		}

		// 跳过登录等接口
		if skipOpLogPaths[c.Request.URL.Path] {
			c.Next()
			return
		}

		// 跳过操作日志自身的写入，防止递归
		if strings.Contains(c.Request.URL.Path, "/operation_log") {
			c.Next()
			return
		}

		bodyStr := operationBody(c)

		// sendCmd 接口中部分 cmd 是查询操作，不记录日志
		if c.Request.URL.Path == "/api/admin/rustdesk/sendCmd" {
			if isReadOnlyCmd(bodyStr) {
				c.Next()
				return
			}
		}

		// 执行后续处理
		c.Next()

		// 异步写入日志，不阻塞响应
		value, _ := c.Get("curUser")
		user, ok := value.(*model.User)
		if !ok || user == nil || user.Id == 0 {
			return
		}

		path := c.FullPath()
		if path == "" {
			path = "<unmatched>"
		}
		ip := c.ClientIP()
		resource, op := parsePath(path)

		// 合并 handler 通过 c.Set("op_log_extra", ...) 补充的实体信息（如删除时请求体只有 ID）
		var extra gin.H
		if v, ok := c.Get("op_log_extra"); ok {
			extra, _ = v.(gin.H)
		}
		record := &model.OperationLog{
			UserId: user.Id, Username: user.Username, Op: op, Resource: resource,
			Detail: operationDetail(path, bodyStr, extra), Ip: ip, Path: path,
		}
		go func() {
			_ = write(record)
		}()
	}
}

// parsePath 从 URL 路径中解析资源名和操作类型
// 例如 /api/admin/user/create → (user, create)
// 例如 /api/admin/peer/batchDelete → (peer, batch_delete)
func parsePath(path string) (resource, op string) {
	// 去掉 /api/admin/ 前缀
	p := strings.TrimPrefix(path, "/api/admin/")
	// 去掉 /my/ 前缀
	p = strings.TrimPrefix(p, "my/")

	parts := strings.Split(p, "/")
	if len(parts) >= 2 {
		resource = parts[0]
		op = parts[1]
	} else if len(parts) == 1 {
		resource = parts[0]
		op = "unknown"
	}

	// 统一操作名格式：camelCase → snake_case（长模式优先匹配）
	op = strings.ReplaceAll(op, "batchCreateFromPeers", "batch_create_from_peers")
	op = strings.ReplaceAll(op, "batchUpdateTags", "batch_update_tags")
	op = strings.ReplaceAll(op, "shareByWebClient", "share_by_web_client")
	op = strings.ReplaceAll(op, "bindConfirm", "bind_confirm")
	op = strings.ReplaceAll(op, "changeCurPwd", "change_cur_pwd")
	op = strings.ReplaceAll(op, "updatePassword", "update_password")
	op = strings.ReplaceAll(op, "batchDelete", "batch_delete")
	op = strings.ReplaceAll(op, "batchCreate", "batch_create")
	op = strings.ReplaceAll(op, "batchUpdate", "batch_update")
	op = strings.ReplaceAll(op, "cmdCreate", "cmd_create")
	op = strings.ReplaceAll(op, "cmdDelete", "cmd_delete")
	op = strings.ReplaceAll(op, "cmdUpdate", "cmd_update")
	op = strings.ReplaceAll(op, "sendCmd", "send_cmd")

	return
}

// isReadOnlyCmd 判断 sendCmd 请求体中的 cmd 是否为只读操作
// 只读命令：
//
//	blocklist/blacklist - 查黑名单
//	u - 查用量
//	rs - 查中继服务器
//	ml - 查必须登录状态
//	h - 查帮助/能力
//	aur - 查中继状态（带 option 时是写操作，不能跳过）
func isReadOnlyCmd(body string) bool {
	// 始终是读操作的命令
	for _, cmd := range []string{"blocklist", "blacklist", "u", "rs", "ml", "h"} {
		if strings.Contains(body, `"cmd":"`+cmd+`"`) ||
			strings.Contains(body, `"cmd": "`+cmd+`"`) {
			return true
		}
	}
	// aur 不带 option 时是读操作
	if (strings.Contains(body, `"cmd":"aur"`) || strings.Contains(body, `"cmd": "aur"`)) &&
		!strings.Contains(body, `"option"`) {
		return true
	}
	return false
}

const maxOperationBodyBytes = 64 << 10

type operationBodyReader struct {
	io.Reader
	io.Closer
}

// Limit the audit copy while leaving the complete body available to the handler.
func operationBody(c *gin.Context) string {
	if c.Request.Body == nil {
		return ""
	}
	original := c.Request.Body
	body, err := io.ReadAll(io.LimitReader(original, maxOperationBodyBytes+1))
	c.Request.Body = operationBodyReader{io.MultiReader(bytes.NewReader(body), original), original}
	if err != nil || len(body) > maxOperationBodyBytes {
		return ""
	}
	return string(body)
}

func operationDetail(path, body string, extra gin.H) string {
	m := map[string]interface{}{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		m = map[string]interface{}{"details": "[OMITTED]"}
	}
	if m == nil {
		m = map[string]interface{}{}
	}
	for k, v := range extra {
		m[k] = v
	}
	// Normalize extra values too, so nested typed maps and structs cannot bypass redaction.
	b, err := json.Marshal(m)
	if err != nil {
		return `{"details":"[OMITTED]"}`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return `{"details":"[OMITTED]"}`
	}
	resource, _ := parsePath(path)
	switch resource {
	case "user", "oauth", "user_token", "peer_strategy", "config":
		metadata := map[string]interface{}{"details": "[OMITTED]"}
		for _, key := range []string{"id", "ids", "username", "peer_id", "alias", "op", "oauth_type"} {
			if value, ok := m[key]; ok && safeOperationMetadata(value) {
				metadata[key] = value
			}
		}
		m = metadata
	default:
		redactOperationValue(m)
	}
	b, err = json.Marshal(m)
	if err != nil {
		return `{"details":"[OMITTED]"}`
	}
	if len(b) > 2000 {
		end := 2000
		for !utf8.Valid(b[:end]) {
			end--
		}
		return string(b[:end]) + "...[truncated]"
	}
	return string(b)
}

func safeOperationMetadata(value interface{}) bool {
	switch value := value.(type) {
	case string:
		return len(value) <= 256
	case float64:
		return true
	case []interface{}:
		for _, item := range value {
			switch item := item.(type) {
			case string:
				if len(item) > 256 {
					return false
				}
			case float64:
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}

func redactOperationValue(value interface{}) {
	switch value := value.(type) {
	case map[string]interface{}:
		for key, field := range value {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
			if strings.Contains(normalized, "password") || strings.Contains(normalized, "secret") || normalized == "hash" ||
				strings.Contains(normalized, "token") || strings.HasSuffix(normalized, "key") ||
				normalized == "authorization" || normalized == "cookie" || normalized == "code" ||
				normalized == "state" || normalized == "nonce" || normalized == "codeverifier" || normalized == "verifier" {
				value[key] = "[REDACTED]"
			} else {
				redactOperationValue(field)
			}
		}
	case []interface{}:
		for _, item := range value {
			redactOperationValue(item)
		}
	}
}
