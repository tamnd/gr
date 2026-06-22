package engine

import (
	"github.com/tamnd/gr/format"
	"github.com/tamnd/gr/store"
)

// CredentialBlob returns the raw bytes of the credential store, or nil when no user
// has ever been created (the section is empty). The bytes are opaque here; package
// cred decodes them into user records. It takes a read lock and does not commit.
func (e *DiskEngine) CredentialBlob() ([]byte, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	head, length, err := e.secs.Get(store.SecCredentials)
	if err != nil {
		return nil, err
	}
	if head == format.NoPage || length == 0 {
		return nil, nil
	}
	log, err := store.OpenLog(e.p, head, int(length))
	if err != nil {
		return nil, err
	}
	out := make([]byte, length)
	if err := log.Read(0, int(length), out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetCredentialBlob rewrites the credential store with blob and commits, the durable
// store behind CREATE/ALTER/DROP USER (doc 18 §10.3). The whole set is small and
// rewritten wholesale on every change, so this writes a fresh Log, points the section
// at it, and frees the old chain, mirroring how a checkpoint replaces a store. Like
// Intern it is its own durable transaction: it takes the write lock and commits.
func (e *DiskEngine) SetCredentialBlob(blob []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	oldHead, oldLen, err := e.secs.Get(store.SecCredentials)
	if err != nil {
		return err
	}
	log, err := store.CreateLog(e.p, format.PageTypeCreds)
	if err != nil {
		return err
	}
	if len(blob) > 0 {
		if _, err := log.Append(blob); err != nil {
			return err
		}
	}
	if err := e.secs.Set(store.SecCredentials, log.Head(), uint64(log.Len())); err != nil {
		return err
	}
	// Free the previous chain now that the section points at the new one, so the old
	// pages return to the free list rather than leaking on every credential change.
	if oldHead != format.NoPage {
		old, err := store.OpenLog(e.p, oldHead, int(oldLen))
		if err != nil {
			return err
		}
		if err := old.Free(); err != nil {
			return err
		}
	}
	_, err = e.commitPager()
	return err
}
