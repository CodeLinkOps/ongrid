package knowledge

import "testing"

// TestExtractSSHTarget 覆盖 host/owner/repo 这个更具体的匹配键。
//
// 它存在的理由：GitHub 的 deploy key 是**按仓库**发的，所以同一台 host
// 上有两个私有仓库时必然是两把 key。只按 host 匹配会让第一把通吃，第二个
// 仓库克隆失败并报 "Repository not found" —— 那个错误读起来像 URL 写错了，
// 而不是「递错了钥匙」。
func TestExtractSSHTarget(t *testing.T) {
	cases := map[string]string{
		"git@github.com:CodeLinkOps/infra.git":       "github.com/codelinkops/infra",
		"git@github.com:CodeLinkOps/atlas":           "github.com/codelinkops/atlas",
		"ssh://git@github.com/CodeLinkOps/infra.git": "github.com/codelinkops/infra",
		"ssh://git@gitlab.local:2222/g/p.git":        "gitlab.local/g/p",
		// 没有路径时退化成裸 host，不返回空
		"git@github.com:": "github.com",
		// 非 ssh URL 没有 host，返回空
		"https://github.com/CodeLinkOps/infra.git": "",
	}
	for in, want := range cases {
		if got := extractSSHTarget(in); got != want {
			t.Errorf("extractSSHTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExtractSSHTargetKeepsHostFallback 保证裸 host 仍然能被单独取出 ——
// 匹配是两级的，具体键不中时要退回 host。
func TestExtractSSHTargetKeepsHostFallback(t *testing.T) {
	const url = "git@github.com:CodeLinkOps/infra.git"
	if got := extractSSHHost(url); got != "github.com" {
		t.Fatalf("extractSSHHost(%q) = %q，退回用的裸 host 不能丢", url, got)
	}
}
