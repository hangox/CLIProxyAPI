package claude

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestClaudeCompactWithoutSessionIdentityDoesNotCommitOpaqueState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	storePath := filepath.Join(dir, "compact.db")
	keyringPath := filepath.Join(dir, "compact.key")
	cfg := &sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{Compact: sdkconfig.ClaudeCompactConfig{
		Enabled: true, Protocol: "v2", StorePath: storePath, KeyringFile: keyringPath, TTL: time.Hour, Capacity: 8, MaxBytes: 1 << 20,
	}}}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	fixture := readClaudeCompactFixtureFromRoot(t)
	out, handled := handler.handleClaudeCompact(ctx, fixture)
	if !handled {
		t.Fatal("sessionless compact request was not handled")
	}
	if strings.Contains(string(out), "codex-opaque-state:") {
		t.Fatalf("sessionless compact returned opaque marker: %s", out)
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("sessionless compact touched store path: err=%v", err)
	}
	if _, err := os.Stat(keyringPath); !os.IsNotExist(err) {
		t.Fatalf("sessionless compact touched keyring path: err=%v", err)
	}
}
