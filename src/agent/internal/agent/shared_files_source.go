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

// sharedUploadsMu guards the manifest. Uploads and deletes also hold it while
// they publish or remove a file, so a delete that is about to drop a name's
// record cannot remove the record of a newer upload that took the same name.
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

// recordSharedUploadLocked marks rel as shared by the user. A failure only
// loses the label; the upload itself has already been published. The caller
// holds sharedUploadsMu across publishing rel and this call.
func recordSharedUploadLocked(root *os.Root, rel string) {
	uploads := readSharedUploads(root)
	uploads[rel] = true
	_ = writeSharedUploads(root, uploads)
}

// forgetSharedUploadLocked drops rel after a delete, so a file the Agent later
// writes under the same name is not labelled as the user's. The caller holds
// sharedUploadsMu across removing rel and this call.
func forgetSharedUploadLocked(root *os.Root, rel string) {
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
