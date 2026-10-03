package node

import (
	"errors"
	"github.com/ictusidera/mediatrix/internal/model"
	"github.com/ictusidera/mediatrix/internal/store"
)

func storageError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return model.Err("not_found", "file not found")
	case errors.Is(err, store.ErrTooLarge):
		return model.Err("too_large", "file exceeds size limit")
	case errors.Is(err, store.ErrStoreFull):
		return model.Err("too_large", "content store capacity exceeded")
	case errors.Is(err, store.ErrHashMismatch), errors.Is(err, store.ErrCorrupt):
		return model.Err("integrity", "file integrity verification failed")
	case errors.Is(err, store.ErrClosed):
		return model.Err("unavailable", "node is closing")
	default:
		return err
	}
}
