package agent

import (
	"encoding/json"
	"os"
	"slices"
	"sync"
)

// Uploads through POST /api/files are recorded here so listings can tell
// files the user shared apart from files the Agent wrote. The name starts with
// a dot, so listings, downloads and deletes never expose it.
const sharedUploadsManifest = ".uploads.json"

const (
	sharedByUser = "user"
	sharedByAico = "aico"
)

// Listings, uploads and deletes run concurrently; the manifest is one file.
var sharedUploadsMu sync.Mutex

func readSharedUploads(root *os.Root) map[string]bool {
	uploads := map[string]bool{}
	data, err := root.ReadFile(sharedUploadsManifest)
	if err != nil {
		return uploads
	}
	var paths []string
	if json.Unmarshal(data, &paths) == nil {
		for _, rel := range paths {
			uploads[rel] = true
		}
	}
	return uploads
}

func writeSharedUploads(root *os.Root, uploads map[string]bool) error {
	paths := make([]string, 0, len(uploads))
	for rel := range uploads {
		paths = append(paths, rel)
	}
	slices.Sort(paths)
	data, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	temp := sharedUploadsManifest + ".tmp"
	if err := root.WriteFile(temp, data, 0600); err != nil {
		return err
	}
	return root.Rename(temp, sharedUploadsManifest)
}

// recordSharedUpload marks rel as shared by the user. A failure only loses
// the label; the upload itself has already been published.
func recordSharedUpload(root *os.Root, rel string) {
	sharedUploadsMu.Lock()
	defer sharedUploadsMu.Unlock()
	uploads := readSharedUploads(root)
	uploads[rel] = true
	_ = writeSharedUploads(root, uploads)
}

// forgetSharedUpload drops rel after a delete, so a file the Agent later
// writes under the same name is not labelled as the user's.
func forgetSharedUpload(root *os.Root, rel string) {
	sharedUploadsMu.Lock()
	defer sharedUploadsMu.Unlock()
	uploads := readSharedUploads(root)
	if !uploads[rel] {
		return
	}
	delete(uploads, rel)
	_ = writeSharedUploads(root, uploads)
}

func sharedFileSource(uploads map[string]bool, rel string) string {
	if uploads[rel] {
		return sharedByUser
	}
	return sharedByAico
}
