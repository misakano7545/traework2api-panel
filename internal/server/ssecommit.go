// ssecommit.go 记录「SSE 流是否已经对客户端开过口」，并在已开口之后把错误降级成
// 协议内的 error 帧。
//
// 为什么需要：首帧心跳（upstream/solosse.go 的 headKeepaliveInterval）会在等上游第一个
// 数据帧的空窗里先写 `: keepalive` 注释帧——它提交了 200，于是「这次尝试彻底失败、
// 全部号都不可用」时再走 writeOpenAIError 的 JSON 分支，就会把一段裸 JSON 追加进
// 已经开始的 SSE 流：SSE 客户端要么当成脏帧忽略（表现为「流没头没尾地断在那」），
// 要么解析报错整轮失败。裸 JSON 与注释帧在线上看起来一模一样，都是「莫名断了」。
//
// 语义与参考实现 workbuddy2api 的 sseCommitWriter 一致：
//   - 未提交：照旧真实状态码 + JSON 错误体（客户端能按 HTTP 语义重试）；
//   - 已提交：只能在协议内收尾 —— event: error + data: {"error":…} + data: [DONE]，
//     状态码已是 200，改不了。
//
// 并发：所有写入都发生在同一条请求的写路径上（上游 StreamWithError 的写锁退出后
// 才轮到 handler 写错误体），所以这里不再加锁。
package server

import (
	"encoding/json"
	"io"
	"net/http"
)

// sseCommitWriter 包一层 ResponseWriter，记住有没有写过字节（含注释帧）。
type sseCommitWriter struct {
	http.ResponseWriter
	wrote bool
}

func (c *sseCommitWriter) WriteHeader(code int) {
	c.wrote = true
	c.ResponseWriter.WriteHeader(code)
}

func (c *sseCommitWriter) Write(p []byte) (int, error) {
	c.wrote = true
	return c.ResponseWriter.Write(p)
}

func (c *sseCommitWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 与测试能拿到原始 writer。
func (c *sseCommitWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// committed 响应是否已经开始（写过任何字节）。
func (c *sseCommitWriter) committed() bool { return c.wrote }

// writeSSEErrorFrame 在已开始的 SSE 流里收尾：error 事件 + [DONE]。
//
// 帧格式与 upstream 侧流内错误一致（OpenAI 对象信封）：agent 侧的 SDK（openai-python
// 等）只在 data 里看到 error 键才会抛 APIError，裸字符串会被解析成「模型返回空」。
func writeSSEErrorFrame(w http.ResponseWriter, status int, code, msg string) {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "api_error",
		"code":    code,
		"status":  status,
	}})
	_, _ = io.WriteString(w, "event: error\ndata: "+string(body)+"\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
