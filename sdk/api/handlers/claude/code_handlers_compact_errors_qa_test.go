package claude

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecompact"
)

func TestClaudeCompactErrorResponseIncludesStableCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	handler.writeCompactError(ctx, &claudecompact.Error{Code: claudecompact.ErrSessionMismatch, Status: 400, PublicText: "compact marker session mismatch"})
	if recorder.Code != 400 {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if got := recorder.Body.String(); got == "" || !containsJSONField(got, "code", string(claudecompact.ErrSessionMismatch)) {
		t.Fatalf("error response = %s, want stable error.code", got)
	}
}

func containsJSONField(body, field, value string) bool {
	return strings.Contains(body, `"`+field+`":"`+value+`"`)
}
