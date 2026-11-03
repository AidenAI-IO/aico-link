package agent

import (
	"strings"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
)

// userProfileSectionHeader opens the section of the system message that lists
// the user's saved profile, preference, and rule memories. The section is
// always the last part of the system message so it can be replaced without
// rebuilding the rest of the prompt, whose date and skills have their own
// refresh paths.
const userProfileSectionHeader = "\n\n## User profile\n" +
	"The user saved these profile entries, preferences, and rules. Apply them whenever relevant without asking again. " +
	"The user's words in this conversation take precedence. Use recall_memory for other saved facts.\n"

// withUserProfileSection returns the system message with its user profile
// section replaced by profile, or removed when profile is empty.
func withUserProfileSection(systemMessage, profile string) string {
	if index := strings.Index(systemMessage, userProfileSectionHeader); index >= 0 {
		systemMessage = systemMessage[:index]
	}
	if profile = strings.TrimSpace(profile); profile == "" {
		return systemMessage
	}
	return systemMessage + userProfileSectionHeader + profile
}

// refreshUserProfileSection rewrites the session's system message when the
// saved user profile differs from the one it carries, for example after the
// user saved, changed, or forgot a profile memory. The conversation continues
// in a new revision, the same way context compaction rewrites it, so the old
// transcript stays on disk and scheduled tasks keep following the session
// lineage. An unchanged profile leaves the session, and any provider prompt
// cache, untouched.
func refreshUserProfileSection(manager *contextmanager.ContextManager, profile string) error {
	if manager == nil {
		return nil
	}
	messageList, version := manager.MessageSnapshot()
	if len(messageList) == 0 || messageList[0].Role != messages.MessageRoleSystem {
		return nil
	}
	updated := withUserProfileSection(messageList[0].Content, profile)
	if updated == messageList[0].Content {
		return nil
	}
	messageList[0].Content = updated
	// A revision starts a fresh provider conversation; a Responses ID would
	// chain the rewritten transcript onto stale provider state.
	for i := range messageList {
		messageList[i].ResponsesResponseID = ""
	}
	candidate, err := contextmanager.NewContextManagerRevisionFromMessageListAtVersion(manager, messageList, version)
	if err != nil {
		return err
	}
	return manager.Activate(candidate)
}
