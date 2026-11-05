package gitcmd

import (
	"context"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// RepositoryObjectHash and CommonGitDir expose the attr.tree object-format detection to the tests of
// package gitcmd_test, which compare it with what the bundled git reports.
var (
	RepositoryObjectHash = repositoryObjectHash
	CommonGitDir         = commonGitDir
)

// CaptureInvocation returns an option that stores the invocation of the command it configures. It
// comes after the option that configures the hooks. The last call is the command that runs.
func CaptureInvocation(invocation *string) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, cc *cmdCfg) error {
		*invocation = cc.invocation
		return nil
	}
}
