// banyan is the new CLI benchmarked against Scion's `sl` (docs/05-cli.md):
// smart remote control + a git-compatible local repo (a promisor remote pointing back at banyand).
package main

import (
	"fmt"
	"os"
)

const usage = `banyan — banyan 的客户端（骨架阶段）

用法:
  banyan clone <url>[/subdir]     克隆仓库或任意子目录（M1）
  banyan fetch-objects <oid>...   逐对象批量获取（懒回填，M1）
  banyan log [path]               服务端 log（M2）
  banyan blame <path>             服务端 blame（M3）
  banyan diff [rev [path]]        服务端 diff（M2）
  banyan file-history <path>      路径历史（M2）
  banyan snapshot                 快照式本地状态（M4）
  banyan stack                    栈式变更（M4）

骨架阶段仅解析参数并展示计划；M1 起 clone/fetch-objects 生效。
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "clone":
		// TODO(M1): git clone --filter=blob:none <url> (virtual repo URLs auto-expand
		// the "//" form) + configure remote.promisor/partialclonefilter +
		// an object-missing backfill hook.
		fmt.Printf("clone: not implemented yet (M1); url=%v\n", args)
	case "fetch-objects":
		// TODO(M1): POST /api/v1/objects locate + fetch (docs/05 RPC contract).
		fmt.Printf("fetch-objects: not implemented yet (M1); oids=%v\n", args)
	case "log", "blame", "diff", "file-history", "snapshot", "stack":
		fmt.Printf("%s: not implemented yet (see docs/06-roadmap.md)\n", cmd)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}
