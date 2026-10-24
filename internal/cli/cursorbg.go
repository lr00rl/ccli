package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/lr00rl/ccli/internal/cursorusage"
	"github.com/lr00rl/ccli/internal/render"
	"github.com/lr00rl/ccli/internal/store"
)

func init() {
	commands[cursorRefreshCmd] = cmdCursorRefresh
}

// cursorRefreshCmd is the command a command starts, on its own and
// detached, to update Cursor usage while it shows the cache. It is left out
// of the usage text.
const cursorRefreshCmd = "cursor-refresh"

// startRefresh starts it. Tests replace it, so they start no process.
var startRefresh = func() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, cursorRefreshCmd)
	// A session of its own: closing the terminal does not stop the read.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// cmdCursorRefresh reads Cursor usage into the cache, unless another process
// already is. A failure is kept in the cache, where commands report it.
func cmdCursorRefresh(ctx context.Context, a *app, args []string) error {
	dataDir := store.DataDir()
	release, ok := cursorusage.Lock(dataDir)
	if !ok {
		return nil
	}
	defer release()
	_, err := cursorusage.RefreshOption(ctx, dataDir, cursorusage.Option{})
	return err
}

// updateCursorLater starts the cache's update in the background, so the
// next command has fresh usage and this one shows the cache without
// waiting. Usage a day old, or an update that failed, is said on stderr.
func (a *app) updateCursorLater(dataDir string, cur *cursorusage.Ledger) {
	age := render.NewTerm(a.errw, "never").Ago(cur.FetchedAt)
	switch {
	case cur.Error != "":
		fmt.Fprintf(a.errw, "ccli: Cursor usage is from %s; the last update failed: %s\n", age, cur.Error)
	case time.Since(cur.FetchedAt) > 24*time.Hour:
		fmt.Fprintf(a.errw, "ccli: Cursor usage is from %s; it is updating in the background\n", age)
	}
	if cursorusage.Busy(dataDir) {
		a.logf(1, "cursor usage: cache from %s; an update is running", age)
		return
	}
	if err := startRefresh(); err != nil {
		a.logf(1, "cursor usage: could not start an update: %v", err)
		return
	}
	a.logf(1, "cursor usage: cache from %s; updating it in the background", age)
}
