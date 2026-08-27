package knowledge

import (
	"testing"
	"time"
)

func TestHashImportRequestBindsNotesyncResolutionIdentity(t *testing.T) {
	command := ImportCommand{
		ExpectedParentProvided: true,
		Source:                 "notesync",
		NotesyncResolution: &NotesyncImportResolution{
			ReviewID: "10000000-0000-4000-8000-000000000001", BasisHash: "basis",
			DeviceID: "10000000-0000-4000-8000-000000000002", OperationID: "10000000-0000-4000-8000-000000000003",
			RequestHash: "request-a", Kind: "accept_remote", CanonicalPath: "topic.md",
			ExpectedDocumentID:     "10000000-0000-4000-8000-000000000004",
			ObservedRemoteMarkdown: "remote-a", ObservedRemoteVersion: 1, ResolvedAt: time.Unix(1, 0),
		},
	}
	base := hashImportRequest(command, nil)

	changedKind := command
	changedKind.NotesyncResolution = cloneNotesyncImportResolution(command.NotesyncResolution)
	changedKind.NotesyncResolution.Kind = "merged"
	if got := hashImportRequest(changedKind, nil); got == base {
		t.Fatal("knowledge import hash did not bind notesync resolution kind")
	}

	changedRequest := command
	changedRequest.NotesyncResolution = cloneNotesyncImportResolution(command.NotesyncResolution)
	changedRequest.NotesyncResolution.RequestHash = "request-b"
	if got := hashImportRequest(changedRequest, nil); got == base {
		t.Fatal("knowledge import hash did not bind notesync resolution request")
	}

	retry := command
	retry.NotesyncResolution = cloneNotesyncImportResolution(command.NotesyncResolution)
	retry.NotesyncResolution.ObservedRemoteMarkdown = "remote-b"
	retry.NotesyncResolution.ObservedRemoteVersion = 9
	retry.NotesyncResolution.ResolvedAt = time.Unix(9, 0)
	if got := hashImportRequest(retry, nil); got != base {
		t.Fatal("retry-only remote observation or timestamp changed knowledge import hash")
	}
}

func cloneNotesyncImportResolution(value *NotesyncImportResolution) *NotesyncImportResolution {
	cloned := *value
	return &cloned
}
