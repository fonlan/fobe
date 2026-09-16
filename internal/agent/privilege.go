package agent

import "os"

// privileged reports whether this process may do the root-only parts of the
// job: write units under /etc, talk to the system service bus, migrate the
// root-owned legacy layouts. It is a var so tests can flip the deployment mode
// without creating real users.
//
// 非特权模式（design §5.3 实现修订 2026-09-16）：euid != 0 的 agent 把 sing-box
// 收进 fallback spawn 分支、把布局收进 -config 所在目录——非 root 不可能
// systemctl，而入站端口本来就被面板钉在 10000–60000（§9.3），无需任何能力位。
var privileged = func() bool { return os.Geteuid() == 0 }
