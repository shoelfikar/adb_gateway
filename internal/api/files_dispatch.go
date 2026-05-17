// Package api — files_dispatch.go implements D-FB-01 op-verb dispatch:
//
//	GET    /files                          -> DownloadFile (Phase 3 default, ?op= absent)
//	GET    /files?op=list                  -> ListFiles
//	GET    /files?op=stat                  -> StatFile
//	GET    /files?op=download-folder       -> DownloadFolder
//	POST   /files                          -> UploadFile (Phase 3 default, ?op= absent)
//	POST   /files?op=mkdir                 -> MkdirFile
//	POST   /files?op=upload-folder         -> UploadFolder
//	PATCH  /files?op=rename                -> RenameFile
//	DELETE /files                          -> DeleteFile (Phase 3 single-file)
//	DELETE /files?recursive=1              -> DeleteFile (recursive branch added in plan 02)
//
// Backward compatibility: when ?op= is absent on GET/POST/DELETE, Phase 3
// single-file semantics are preserved exactly. PATCH always requires ?op=rename
// (no Phase 3 PATCH on /files existed).
package api

import "net/http"

// FilesDispatcher routes /files requests based on the ?op= query param (D-FB-01).
type FilesDispatcher struct {
	list, stat, download, mkdir, upload, uploadFolder, downloadFolder, rename, delete http.HandlerFunc
}

// NewFilesDispatcher constructs a dispatcher with all handler slots.
func NewFilesDispatcher(
	list, stat, download, mkdir, upload, uploadFolder, downloadFolder, rename, deleteHandler http.HandlerFunc,
) *FilesDispatcher {
	return &FilesDispatcher{
		list: list, stat: stat, download: download,
		mkdir: mkdir, upload: upload, uploadFolder: uploadFolder, downloadFolder: downloadFolder,
		rename: rename, delete: deleteHandler,
	}
}

// Get dispatches GET /files based on ?op= query param.
func (d *FilesDispatcher) Get(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("op") {
	case "list":
		d.list.ServeHTTP(w, r)
	case "stat":
		d.stat.ServeHTTP(w, r)
	case "download-folder":
		d.downloadFolder.ServeHTTP(w, r)
	case "": // Phase 3 backward compat
		d.download.ServeHTTP(w, r)
	default:
		writeError(w, &DomainError{
			Code:       "UNSUPPORTED_OP",
			HTTPStatus: http.StatusBadRequest,
			Message:    "Unknown op: " + r.URL.Query().Get("op"),
		})
	}
}

// Post dispatches POST /files based on ?op= query param.
func (d *FilesDispatcher) Post(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("op") {
	case "mkdir":
		d.mkdir.ServeHTTP(w, r)
	case "upload-folder":
		d.uploadFolder.ServeHTTP(w, r)
	case "": // Phase 3 backward compat
		d.upload.ServeHTTP(w, r)
	default:
		writeError(w, &DomainError{
			Code:       "UNSUPPORTED_OP",
			HTTPStatus: http.StatusBadRequest,
			Message:    "Unknown op: " + r.URL.Query().Get("op"),
		})
	}
}

// Patch dispatches PATCH /files — only op=rename is valid.
func (d *FilesDispatcher) Patch(w http.ResponseWriter, r *http.Request) {
	op := r.URL.Query().Get("op")
	if op == "rename" {
		d.rename.ServeHTTP(w, r)
		return
	}
	writeError(w, &DomainError{
		Code:       "UNSUPPORTED_OP",
		HTTPStatus: http.StatusBadRequest,
		Message:    "PATCH /files requires op=rename",
	})
}

// Delete dispatches DELETE /files. Unknown ?op= values are rejected (W6).
// Phase 3 DELETE takes no ?op=; Phase 03.1 adds ?recursive=1 (not an op).
func (d *FilesDispatcher) Delete(w http.ResponseWriter, r *http.Request) {
	if op := r.URL.Query().Get("op"); op != "" {
		writeError(w, &DomainError{
			Code:       "UNSUPPORTED_OP",
			HTTPStatus: http.StatusBadRequest,
			Message:    "DELETE /files does not accept op=" + op + " (use ?recursive=1 for recursive delete)",
		})
		return
	}
	// DeleteFile internally branches on ?recursive=1 (plan 02 task 3).
	d.delete.ServeHTTP(w, r)
}