package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// Props stamped on posts created or moved by this plugin.
const (
	// ricThreadRootIDProp links a /ric channel echo post to the root of the thread it echoes.
	ricThreadRootIDProp = "ric_thread_root_id"

	// ricRedirectedFromProp marks a reply that was moved out of the stray thread
	// hanging off a /ric echo post, recording the stray post it replaces.
	ricRedirectedFromProp = "ric_redirected_from"
)

// Plugin implements the interface expected by the Mattermost server to communicate between the server and plugin processes.
type Plugin struct {
	plugin.MattermostPlugin

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration.
	configuration *configuration
}

// OnActivate is invoked when the plugin is activated.
func (p *Plugin) OnActivate() error {
	if err := p.API.RegisterCommand(&model.Command{
		Trigger:          "ric",
		AutoComplete:     true,
		AutoCompleteDesc: "Reply in channel from a thread (usage: /ric your message)",
		AutoCompleteHint: "[message]",
	}); err != nil {
		return fmt.Errorf("failed to register /ric command: %w", err)
	}

	p.API.LogInfo("Reply in Channel: Activated")
	return nil
}

// ExecuteCommand handles the /ric slash command.
func (p *Plugin) ExecuteCommand(c *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if args.Command == "" || !strings.HasPrefix(args.Command, "/ric") {
		return nil, nil
	}

	message := strings.TrimSpace(strings.TrimPrefix(args.Command, "/ric"))
	if message == "" {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Usage: `/ric your message here`",
		}, nil
	}

	if args.RootId == "" {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "The `/ric` command must be used from within a thread.",
		}, nil
	}

	// Get the root post of the thread
	rootPost, appErr := p.API.GetPost(args.RootId)
	if appErr != nil {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Could not find the thread's root post.",
		}, nil
	}

	// If this command was issued from a stray thread hanging off an echo post,
	// operate on the real thread the echo post refers to instead: quote, thread
	// reply and KV mappings all use the real root. Without this promotion the
	// paired thread reply would be created in the stray thread and immediately
	// redirected — which loses its attachments (a FileInfo can only ever be
	// attached to a post once) and breaks the edit-sync pairing.
	if realRootID, _ := rootPost.GetProp(ricThreadRootIDProp).(string); realRootID != "" && realRootID != rootPost.Id {
		if realRoot, realErr := p.API.GetPost(realRootID); realErr == nil {
			rootPost = realRoot
		} else {
			p.API.LogWarn("Could not load original thread root, staying in current thread", "root_id", args.RootId, "original_root_id", realRootID, "error", realErr.Error())
		}
	}

	// Get the root post author
	rootAuthor, appErr := p.API.GetUser(rootPost.UserId)
	if appErr != nil {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Could not find the original author.",
		}, nil
	}

	// Attempt to recover orphaned file attachments from the slash command.
	// Mattermost uploads files but doesn't pass FileIds to CommandArgs,
	// so we search for recent unattached files by this user in this channel.
	orphanedFileIds := p.findOrphanedFiles(args.UserId, args.ChannelId)

	// Create channel post first without permalink
	channelPost := &model.Post{
		UserId:    args.UserId,
		ChannelId: args.ChannelId,
		Message:   formatQuotedReply(rootAuthor.Username, rootPost.Message, message, ""),
	}
	channelPost.AddProp(ricThreadRootIDProp, rootPost.Id)

	// Attach recovered files to the channel post (use copies so originals go to thread)
	if len(orphanedFileIds) > 0 {
		copiedFileIds, copyErr := p.API.CopyFileInfos(args.UserId, orphanedFileIds)
		if copyErr != nil {
			p.API.LogWarn("Failed to copy file infos for channel post", "error", copyErr.Error())
		} else {
			channelPost.FileIds = copiedFileIds
			p.API.LogDebug("Attached copied files to channel post", "fileIds", fmt.Sprintf("%v", copiedFileIds))
		}
	}

	createdChannelPost, appErr := p.API.CreatePost(channelPost)
	if appErr != nil {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Failed to post reply in channel.",
		}, nil
	}

	// Store mapping from thread root ID → channel post IDs so MessageHasBeenPosted can find them
	p.addRicPostMapping(rootPost.Id, createdChannelPost.Id)

	// Create the thread reply with a link to the channel post
	channelPostLink := p.buildPermalink(createdChannelPost, args.UserId)
	channel, _ := p.API.GetChannel(args.ChannelId)

	threadMessage := message
	if channelPostLink != "" && channel != nil {
		channelRef := formatChannelRef(channel, channelPostLink)
		threadMessage = fmt.Sprintf("%s\n\n> %s", message, channelRef)
	}

	threadPost := &model.Post{
		UserId:    args.UserId,
		ChannelId: args.ChannelId,
		RootId:    rootPost.Id,
		Message:   threadMessage,
	}

	// Attach original orphaned files to the thread post
	if len(orphanedFileIds) > 0 {
		threadPost.FileIds = model.StringArray(orphanedFileIds)
		p.API.LogDebug("Attached original files to thread post", "fileIds", fmt.Sprintf("%v", orphanedFileIds))
	}

	createdThreadPost, threadErr := p.API.CreatePost(threadPost)
	if threadErr != nil {
		p.API.LogWarn("Failed to create thread reply", "error", threadErr.Error())
	}

	// Now update the channel post with a permalink to the thread reply
	if createdThreadPost != nil {
		threadPostLink := p.buildPermalink(createdThreadPost, args.UserId)
		if threadPostLink != "" {
			createdChannelPost.Message = formatQuotedReply(rootAuthor.Username, rootPost.Message, message, threadPostLink)
			if _, updateErr := p.API.UpdatePost(createdChannelPost); updateErr != nil {
				p.API.LogWarn("Failed to update channel post with thread link", "error", updateErr.Error())
			}
		}

		// Store bidirectional mapping so edits can be synced between the two posts
		p.storePostPair(createdChannelPost.Id, createdThreadPost.Id)
	}

	return &model.CommandResponse{}, nil
}

// MessageHasBeenPosted updates /ric channel posts with a "view newer replies" link
// when new replies are added to a thread that was previously shared via /ric.
func (p *Plugin) MessageHasBeenPosted(c *plugin.Context, post *model.Post) {
	// Only care about thread replies
	if post.RootId == "" {
		return
	}

	// Skip posts created by /ric (avoid update loops)
	if post.GetProp(ricThreadRootIDProp) != nil {
		return
	}

	// Replies that landed in the stray thread under a /ric echo post belong in
	// the original thread: move them there instead of growing that stray thread.
	// A moved copy is never redirected again, but like any other thread reply
	// it flows through the "view newer replies" logic below.
	if post.GetProp(ricRedirectedFromProp) == nil && p.redirectReplyToOriginalThread(post) {
		return
	}
	if strings.Contains(post.Message, "Also sent to") {
		return
	}

	// Look up channel posts created by /ric for this thread
	channelPostIDs := p.getRicPostMappings(post.RootId)
	if len(channelPostIDs) == 0 {
		return
	}

	// Build permalink to the newest reply
	permalink := p.buildPermalink(post, post.UserId)
	if permalink == "" {
		return
	}

	// Update each /ric channel post with the "view newer replies" link
	for _, channelPostID := range channelPostIDs {
		channelPost, appErr := p.API.GetPost(channelPostID)
		if appErr != nil {
			p.API.LogWarn("Failed to get /ric channel post", "post_id", channelPostID, "error", appErr.Error())
			continue
		}

		channelPost.Message = updateNewerRepliesLink(channelPost.Message, permalink)
		if _, updateErr := p.API.UpdatePost(channelPost); updateErr != nil {
			p.API.LogWarn("Failed to update /ric channel post", "post_id", channelPostID, "error", updateErr.Error())
		}
	}
}

// redirectReplyToOriginalThread moves a reply that was posted inside the stray
// thread hanging off a /ric channel echo post into the original thread the echo
// post refers to. The stray post is soft-deleted (audit trail), and the replier
// gets an ephemeral note explaining where their reply went — visible only to
// them, never stored. Returns true when the reply belongs to a /ric echo post
// and has been fully handled here, false for regular replies.
func (p *Plugin) redirectReplyToOriginalThread(post *model.Post) bool {
	echoPost, appErr := p.API.GetPost(post.RootId)
	if appErr != nil {
		return false
	}
	origRootID, _ := echoPost.GetProp(ricThreadRootIDProp).(string)
	if origRootID == "" {
		return false // thread root is a regular post, nothing to redirect
	}
	if origRootID == post.RootId {
		// Possible manual DB edit or imported data; a move here would only churn the reply through a copy-delete cycle inside its own thread, so leave it where the user typed it.
		p.API.LogWarn("ric redirect: echo post's ric_thread_root_id points at itself (should not happen), leaving reply in place", "post_id", post.Id, "root_id", post.RootId)
		return true
	}

	originalRoot, appErr := p.API.GetPost(origRootID)
	if appErr != nil || originalRoot.DeleteAt != 0 {
		p.API.LogWarn("ric redirect: original thread root missing, leaving reply in place", "post_id", post.Id, "original_root_id", origRootID)
		return true
	}

	// Re-create the reply in the original thread, preserving author, content,
	// and timestamps so it reads exactly like a normal thread reply.
	moved := &model.Post{
		UserId:    post.UserId,
		ChannelId: post.ChannelId,
		RootId:    origRootID,
		Message:   post.Message,
		CreateAt:  post.CreateAt,
		UpdateAt:  post.UpdateAt,
	}
	moved.AddProp(ricRedirectedFromProp, post.Id)

	// Attachments must be duplicated, not moved: the store only ever attaches a
	// FileInfo while it is unattached (PostId == "", file_info_store.AttachToPost),
	// so reusing the stray post's file ids would silently create the moved post
	// without files. CopyFileInfos is the same mechanism ExecuteCommand uses.
	if len(post.FileIds) > 0 {
		copiedFileIds, copyErr := p.API.CopyFileInfos(post.UserId, []string(post.FileIds))
		if copyErr != nil {
			// Fail open: a text-only copy of the reply beats no reply at all.
			p.API.LogWarn("ric redirect: failed to copy attachments, moved reply will be text-only", "post_id", post.Id, "error", copyErr.Error())
		} else {
			moved.FileIds = model.StringArray(copiedFileIds)
		}
	}

	created, appErr := p.API.CreatePost(moved)
	if appErr != nil {
		// Fail open: a reply in the wrong thread beats a lost reply.
		p.API.LogWarn("ric redirect: failed to copy reply into original thread, leaving it in place", "post_id", post.Id, "original_root_id", origRootID, "error", appErr.Error())
		return true
	}

	// Mirror any reactions captured during the move window (usually none).
	reactions, rErr := p.API.GetReactions(post.Id)
	if rErr != nil {
		p.API.LogWarn("ric redirect: failed to read reactions on stray reply", "post_id", post.Id, "error", rErr.Error())
	}
	for _, reaction := range reactions {
		mirror := &model.Reaction{
			UserId:    reaction.UserId,
			PostId:    created.Id,
			EmojiName: reaction.EmojiName,
			ChannelId: created.ChannelId, // both posts live in the same channel
		}
		if _, mErr := p.API.AddReaction(mirror); mErr != nil {
			p.API.LogWarn("ric redirect: failed to mirror reaction onto moved reply", "post_id", post.Id, "error", mErr.Error())
		}
	}

	// Soft-delete the stray reply. It keeps its id and a delete marker, and the
	// moved copy records it via the ric_redirected_from prop.
	if appErr := p.API.DeletePost(post.Id); appErr != nil {
		p.API.LogWarn("ric redirect: failed to delete stray reply, it may appear twice", "post_id", post.Id, "error", appErr.Error())
	}

	// Tell the replier (and only the replier) where their message went. The note
	// is channel-level: the stray thread it was written into no longer exists,
	// and the original thread shows the reply itself.
	if permalink := p.buildPermalink(created, post.UserId); permalink != "" {
		p.API.SendEphemeralPost(post.UserId, &model.Post{
			UserId:    post.UserId,
			ChannelId: post.ChannelId,
			Message:   fmt.Sprintf("Your reply to the thread shared in this channel was moved to the [original thread](%s). Only you can see this note.", permalink),
		})
	}

	return true
}

// MessageHasBeenUpdated syncs edits between paired /ric channel posts and thread posts.
func (p *Plugin) MessageHasBeenUpdated(c *plugin.Context, newPost *model.Post, oldPost *model.Post) {
	// Only care about message text changes
	if newPost.Message == oldPost.Message {
		return
	}

	// Look up the paired post
	pairedPostID := p.getPairedPostID(newPost.Id)
	if pairedPostID == "" {
		return
	}

	pairedPost, appErr := p.API.GetPost(pairedPostID)
	if appErr != nil {
		p.API.LogWarn("Failed to get paired post for edit sync", "post_id", pairedPostID, "error", appErr.Error())
		return
	}

	isChannelPost := newPost.GetProp(ricThreadRootIDProp) != nil

	var newMessage string
	if isChannelPost {
		// User edited the channel post — extract the new reply text and update the thread post
		newReplyText := extractReplyFromChannelPost(newPost.Message)
		newMessage = replaceReplyInThreadPost(pairedPost.Message, newReplyText)
	} else {
		// User edited the thread post — extract the new reply text and update the channel post
		newReplyText := extractReplyFromThreadPost(newPost.Message)
		newMessage = replaceReplyInChannelPost(pairedPost.Message, newReplyText)
	}

	// Skip if the paired post already has the same message (prevents infinite loop)
	if pairedPost.Message == newMessage {
		return
	}

	pairedPost.Message = newMessage
	if _, updateErr := p.API.UpdatePost(pairedPost); updateErr != nil {
		p.API.LogWarn("Failed to sync edit to paired post", "post_id", pairedPostID, "error", updateErr.Error())
	}
}

// ReactionHasBeenAdded mirrors a reaction added to one /ric post onto its paired post,
// attributed to the same user, so the channel post and thread post stay in sync.
func (p *Plugin) ReactionHasBeenAdded(c *plugin.Context, reaction *model.Reaction) {
	pairedPostID := p.getPairedPostID(reaction.PostId)
	if pairedPostID == "" {
		return
	}

	// Skip if the paired post already has this reaction (prevents infinite loop:
	// mirroring an add re-fires this hook for the paired post).
	if p.hasReaction(pairedPostID, reaction.UserId, reaction.EmojiName) {
		return
	}

	mirror := &model.Reaction{
		UserId:    reaction.UserId,
		PostId:    pairedPostID,
		EmojiName: reaction.EmojiName,
		ChannelId: reaction.ChannelId, // both paired posts live in the same channel
	}
	if _, appErr := p.API.AddReaction(mirror); appErr != nil {
		p.API.LogWarn("Failed to sync added reaction to paired post", "post_id", pairedPostID, "error", appErr.Error())
	}
}

// ReactionHasBeenRemoved mirrors a reaction removal from one /ric post onto its paired post.
func (p *Plugin) ReactionHasBeenRemoved(c *plugin.Context, reaction *model.Reaction) {
	pairedPostID := p.getPairedPostID(reaction.PostId)
	if pairedPostID == "" {
		return
	}

	// Skip if the paired post no longer has this reaction (prevents infinite loop).
	if !p.hasReaction(pairedPostID, reaction.UserId, reaction.EmojiName) {
		return
	}

	mirror := &model.Reaction{
		UserId:    reaction.UserId,
		PostId:    pairedPostID,
		EmojiName: reaction.EmojiName,
		ChannelId: reaction.ChannelId,
	}
	if appErr := p.API.RemoveReaction(mirror); appErr != nil {
		p.API.LogWarn("Failed to sync removed reaction from paired post", "post_id", pairedPostID, "error", appErr.Error())
	}
}

// hasReaction reports whether postID already has a reaction with the given user and emoji.
func (p *Plugin) hasReaction(postID, userID, emojiName string) bool {
	reactions, appErr := p.API.GetReactions(postID)
	if appErr != nil {
		p.API.LogWarn("Failed to get reactions for paired post", "post_id", postID, "error", appErr.Error())
		return false
	}
	for _, r := range reactions {
		if r.UserId == userID && r.EmojiName == emojiName {
			return true
		}
	}
	return false
}

// storePostPair stores a bidirectional mapping between a channel post and its thread post.
func (p *Plugin) storePostPair(channelPostID, threadPostID string) {
	if appErr := p.API.KVSet("ric_pair:"+channelPostID, []byte(threadPostID)); appErr != nil {
		p.API.LogWarn("Failed to store post pair mapping", "error", appErr.Error())
	}
	if appErr := p.API.KVSet("ric_pair:"+threadPostID, []byte(channelPostID)); appErr != nil {
		p.API.LogWarn("Failed to store post pair mapping", "error", appErr.Error())
	}
}

// getPairedPostID returns the paired post ID for a given post, or empty string if not found.
func (p *Plugin) getPairedPostID(postID string) string {
	data, appErr := p.API.KVGet("ric_pair:" + postID)
	if appErr != nil || data == nil {
		return ""
	}
	return string(data)
}

// kvKeyForThread returns the KV store key for a thread's /ric channel post mappings.
func kvKeyForThread(rootID string) string {
	return "ric:" + rootID
}

// addRicPostMapping appends a channel post ID to the list for a given thread root ID.
func (p *Plugin) addRicPostMapping(rootID, channelPostID string) {
	existing := p.getRicPostMappings(rootID)
	existing = append(existing, channelPostID)

	data, err := json.Marshal(existing)
	if err != nil {
		p.API.LogWarn("Failed to marshal /ric post mapping", "error", err.Error())
		return
	}

	if appErr := p.API.KVSet(kvKeyForThread(rootID), data); appErr != nil {
		p.API.LogWarn("Failed to save /ric post mapping", "error", appErr.Error())
	}
}

// getRicPostMappings returns the list of channel post IDs for a given thread root ID.
func (p *Plugin) getRicPostMappings(rootID string) []string {
	data, appErr := p.API.KVGet(kvKeyForThread(rootID))
	if appErr != nil || data == nil {
		return nil
	}

	var postIDs []string
	if err := json.Unmarshal(data, &postIDs); err != nil {
		p.API.LogWarn("Failed to unmarshal /ric post mapping", "error", err.Error())
		return nil
	}
	return postIDs
}

// OnDeactivate is invoked when the plugin is deactivated.
func (p *Plugin) OnDeactivate() error {
	p.API.LogInfo("Reply in Channel: Deactivated")
	return nil
}
