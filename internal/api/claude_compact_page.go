package api

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed claude_compact_page.html
var claudeCompactPageFS embed.FS

func (s *Server) serveClaudeCompactPage(c *gin.Context) {
	data, err := claudeCompactPageFS.ReadFile("claude_compact_page.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "compact observation page unavailable")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}
