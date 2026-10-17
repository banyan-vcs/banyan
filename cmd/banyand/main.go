// banyand 是 banyan 的服务端守护进程（骨架阶段）：
// 挂载 git smart HTTP v1 端点（gitwire）与 banyan RPC（rpc），
// 单机模式：内存元数据 + 本地盘分片（docs/00 部署形态）。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/rpc"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP 监听地址")
	root := flag.String("root", "./data", "本地分片存储根目录")
	repo := flag.String("init-repo", "", "启动时创建的仓库名（可选）")
	flag.Parse()

	meta := storage.NewMemoryMetaStore()
	local := storage.NewLocalShardStore(*root)
	nodes := []storage.Node{{ID: "local-1", Local: true}}
	objects := storage.NewObjectService(meta, local, nodes, model.SHA1)

	if *repo != "" {
		if err := meta.CreateRepo(nil, *repo, model.SHA1); err != nil {
			log.Fatalf("create repo: %v", err)
		}
		fmt.Fprintf(os.Stderr, "created repo %q (object-format=sha1)\n", *repo)
	}

	mux := http.NewServeMux()
	gitwire.NewServer(meta, objects, model.SHA1).Register(mux)
	rpc.NewServer(meta, objects, model.SHA1).Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "banyan skeleton — endpoints: /r/<repo>[/info/refs|/git-upload-pack|/git-receive-pack], /api/v1/*")
	})

	fmt.Fprintf(os.Stderr, "banyand listening on %s (root=%s)\n", *listen, *root)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
