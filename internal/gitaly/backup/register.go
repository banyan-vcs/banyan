package backup

import (
	"google.golang.org/grpc"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// Register mounts the BackupService (the ID-registry face); the
// RepositoryService backup pair lives in the repository family.
func Register(s *grpc.Server, b *kit.Base) {
	gitalypb.RegisterBackupServiceServer(s, &backupService{Base: b})
}
