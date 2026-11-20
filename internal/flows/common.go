package flows

import (
	"context"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

// LongarmClient is the subset of the longarm CLI adapter the flows use.
type LongarmClient interface {
	Version(ctx context.Context) (longarm.VersionInfo, error)
	WhoAmI(ctx context.Context) (longarm.WhoAmI, error)
	Workers(ctx context.Context) (longarm.WorkerList, error)
	WorkerAdd(ctx context.Context, name string, ttl time.Duration, labels []string) (secret.Secret, error)
	AdminJoinCode(ctx context.Context, req longarm.JoinCodeRequest) (secret.Secret, error)
	AdminApprove(ctx context.Context, name string) error
	Join(ctx context.Context, address string, code secret.Secret, fingerprint string) ([]byte, error)
	DaemonStart(ctx context.Context) error
	Exec(ctx context.Context, worker string, argv ...string) ([]byte, error)
}

var _ LongarmClient = (*longarm.Client)(nil)
