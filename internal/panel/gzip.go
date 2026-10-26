// gzip.go 面板响应压缩。
//
// 为什么要单独做：面板的 app.js 约 310KB、index.html 约 150KB，账号总览在 200 个号时
// 也有几百 KB，而 Go 的 net/http 不会自动压缩。这些内容重复度高（JSON 字段名、CSS），
// gzip 后通常只剩 1/4～1/8，首屏和 5s 轮询的传输量都明显下降。
//
// 只作用于面板自身（/panel/*）：网关的 SSE 流式响应不能整段缓冲/压缩，否则首字延迟
// 和 flush 语义都会变。字体 woff2 本身已压缩，Content-Type 不在 compressible 里，跳过。
package panel

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// gzipResponseWriter 按 Content-Type 决定是否压缩，并对 http.Flusher 透明转发
// （selfupdate 回包后会 Flush 再 exec 换进程，wrapper 不实现 Flush 会推迟这段输出）。
type gzipResponseWriter struct {
	http.ResponseWriter
	gz       *gzip.Writer
	decided  bool
	compress bool
	status   int
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if w.decided {
		return
	}
	w.decided = true
	w.status = code
	// 304/204 没有正文；1xx 也不压缩。
	if code >= 200 && code != http.StatusNoContent && code != http.StatusNotModified {
		if compressibleType(w.Header().Get("Content-Type")) {
			w.Header().Del("Content-Length")
			w.Header().Set("Content-Encoding", "gzip")
			w.gz = gzip.NewWriter(w.ResponseWriter)
			w.compress = true
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.compress {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Flush 让调用方拿到的 http.Flusher 仍然有效：先刷 gzip 缓冲，再刷底层连接。
func (w *gzipResponseWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Close 结束 gzip 流（写出尾部并校验）；未压缩时是空操作。
func (w *gzipResponseWriter) Close() {
	if w.gz != nil {
		_ = w.gz.Close()
		w.gz = nil
	}
}

// compressibleType 只压缩文本类响应：字体/图片已是压缩格式，再压反而更大。
func compressibleType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	switch {
	case strings.HasPrefix(ct, "text/"):
		return true
	case strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "application/javascript"),
		strings.HasPrefix(ct, "application/xml"),
		strings.HasPrefix(ct, "image/svg+xml"):
		return true
	}
	return false
}

// acceptsGzip 解析 Accept-Encoding；q=0 视为拒绝（RFC 9110）。
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			continue
		}
		if strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0") {
			return false
		}
		return true
	}
	return false
}
