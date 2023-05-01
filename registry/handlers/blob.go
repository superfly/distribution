package handlers

import (
	"errors"
	"github.com/distribution/distribution/v3/registry/storage"
	"net/http"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/context"
	"github.com/distribution/distribution/v3/registry/api/errcode"
	v2 "github.com/distribution/distribution/v3/registry/api/v2"
	"github.com/gorilla/handlers"
	"github.com/opencontainers/go-digest"
)

// blobDispatcher uses the request context to build a blobHandler.
func blobDispatcher(ctx *Context, r *http.Request) http.Handler {
	dgst, err := getDigest(ctx)
	if err != nil {

		if err == errDigestNotAvailable {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx.Errors = append(ctx.Errors, v2.ErrorCodeDigestInvalid.WithDetail(err))
			})
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx.Errors = append(ctx.Errors, v2.ErrorCodeDigestInvalid.WithDetail(err))
		})
	}

	blobHandler := &blobHandler{
		Context: ctx,
		Digest:  dgst,
	}

	mhandler := handlers.MethodHandler{
		http.MethodGet:  http.HandlerFunc(blobHandler.GetBlob),
		http.MethodHead: http.HandlerFunc(blobHandler.GetBlob),
	}

	if !ctx.readOnly {
		mhandler[http.MethodDelete] = http.HandlerFunc(blobHandler.DeleteBlob)
	}

	return mhandler
}

// blobHandler serves http blob requests.
type blobHandler struct {
	*Context

	Digest digest.Digest
}

// GetBlob fetches the binary data from backend storage and returns it in the
// response.
// Any public blob will be automatically mounted to the repository.
func (bh *blobHandler) GetBlob(w http.ResponseWriter, r *http.Request) {
	blobDigest := bh.Digest
	logger := context.GetLoggerWithField(bh, "digest", bh.Digest)
	logger.Debug("GetBlob")
	blobs := bh.Repository.Blobs(bh)
	desc, err := blobs.Stat(bh, bh.Digest)
	if err != nil {
		if err == distribution.ErrBlobUnknown {
			logger.Debug("Attempt to auto-mount blob")
			_, err := blobs.Create(bh, storage.WithMount(bh.Digest), storage.WithoutUpload())
			if err != nil {
				var ebm distribution.ErrBlobMounted
				if errors.As(err, &ebm) {
					logger.Debug("Successfully auto-mounted blob")
				} else {
					logger.Debugf("unexpected error auto-mounting blob: %v", err)
					bh.Errors = append(bh.Errors, v2.ErrorCodeBlobUnknown.WithDetail(bh.Digest))
					return
				}
			} else {
				bh.Errors = append(bh.Errors, v2.ErrorCodeBlobUnknown.WithDetail(bh.Digest))
				return
			}
		} else {
			bh.Errors = append(bh.Errors, errcode.ErrorCodeUnknown.WithDetail(err))
			return
		}
	} else {
		blobDigest = desc.Digest
	}

	if err := blobs.ServeBlob(bh, w, r, blobDigest); err != nil {
		logger.Debugf("unexpected error getting blob HTTP handler: %v", err)
		bh.Errors = append(bh.Errors, errcode.ErrorCodeUnknown.WithDetail(err))
		return
	}
}

// DeleteBlob deletes a layer blob
func (bh *blobHandler) DeleteBlob(w http.ResponseWriter, r *http.Request) {
	context.GetLogger(bh).Debug("DeleteBlob")

	blobs := bh.Repository.Blobs(bh)
	err := blobs.Delete(bh, bh.Digest)
	if err != nil {
		switch err {
		case distribution.ErrUnsupported:
			bh.Errors = append(bh.Errors, errcode.ErrorCodeUnsupported)
			return
		case distribution.ErrBlobUnknown:
			bh.Errors = append(bh.Errors, v2.ErrorCodeBlobUnknown)
			return
		default:
			bh.Errors = append(bh.Errors, err)
			context.GetLogger(bh).Errorf("Unknown error deleting blob: %s", err.Error())
			return
		}
	}

	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}
