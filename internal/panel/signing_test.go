package panel

import (
	"testing"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/upstream"
)

func TestPanelInjectsSigningKeyIntoBothOAuthFlows(t *testing.T) {
	up := upstream.New()
	up.AccessKey = "fixture-panel-signing-key"
	p := New(Config{Upstream: up})
	if p.wxOAuth.AccessKey != up.AccessKey || p.qqOAuth.AccessKey != up.AccessKey {
		t.Fatal("both OAuth flows must receive the runtime key")
	}
}
