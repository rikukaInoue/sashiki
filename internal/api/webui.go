// 最小の Web UI: ブランチ一覧(GET /)。API と同じリスナーで返す。
// 外部依存なしの単一ページ。SQL エディタ等は将来。
package api

import (
	_ "embed"
	"net/http"
)

//go:embed webui.html
var webuiHTML []byte

func (s *Server) handleWebUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(webuiHTML)
}
