package contextmanager

import (
	"testing"

	"aiden-agent/internal/agent/messages"
)

func TestHasSessionAncestorFollowsCompactionAndClearedRoot(t *testing.T) {
	folder := t.TempDir()
	root, err := NewContextManagerFromMessageList(folder, []messages.Message{{Role: messages.MessageRoleSystem, Content: "system"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := NewContextManagerRevisionFromMessageList(root, root.CloneMessageList())
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := NewContextManagerRevisionFromMessageList(child, child.CloneMessageList())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{root.GetSessionID(), child.GetSessionID(), grandchild.GetSessionID()} {
		ok, err := grandchild.HasSessionAncestor(id)
		if err != nil || !ok {
			t.Fatalf("HasSessionAncestor(%q) = %v, %v", id, ok, err)
		}
	}
	if ok, err := grandchild.HasSessionAncestor("unrelated"); err != nil || ok {
		t.Fatalf("unrelated = %v, %v", ok, err)
	}

	cleared, err := NewContextManagerCandidate(folder, "new root")
	if err != nil {
		t.Fatal(err)
	}
	if err := grandchild.Activate(cleared); err != nil {
		t.Fatal(err)
	}
	if ok, err := grandchild.HasSessionAncestor(root.GetSessionID()); err != nil || ok {
		t.Fatalf("cleared root still matched: %v, %v", ok, err)
	}
}
