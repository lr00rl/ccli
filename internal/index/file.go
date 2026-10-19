package index

import (
	"io/fs"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/store"
)

func storeFile(a account.Account, path, session, kind, agent string, st fs.FileInfo) store.File {
	return store.File{
		Path: path, Account: a.Name, SessionID: session, Kind: kind, AgentID: agent,
		Size: st.Size(), MTime: st.ModTime().UnixMilli(), Born: birth(path, st),
	}
}
