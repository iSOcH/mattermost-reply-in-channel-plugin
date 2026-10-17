package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// Cast and fixture identities. Tests build posts from these and assert against
// them, so renaming or reshaping a fixture is a one-line change.
const (
	echoID  = "echo1"  // channel-level /ric echo post
	rootID  = "root1"  // original thread root the echo post refers to
	replyID = "reply1" // stray reply under the echo post
	movedID = "moved1" // copy the redirect creates in the original thread

	chanID = "chan1"
	teamID = "team1"

	echoAuthor = "alice" // who ran /ric
	rootAuthor = "bob"   // original thread author
	replier    = "carol" // typed into the stray thread
	reactor    = "dave"

	fileID       = "file1"
	copiedFileID = "file1-copy"

	siteURL  = "http://localhost:8065"
	teamName = "myteam"
)

// newEchoPost is a /ric channel echo post linked to the original thread root.
// Fresh instance per call: tests and the plugin mutate posts in place.
func newEchoPost() *model.Post {
	post := &model.Post{Id: echoID, UserId: echoAuthor, ChannelId: chanID}
	post.AddProp(ricThreadRootIDProp, rootID)
	return post
}

func newOriginalRoot() *model.Post {
	return &model.Post{Id: rootID, UserId: rootAuthor, ChannelId: chanID}
}

// newStrayReply is a reply typed into the stray thread hanging off the echo post.
func newStrayReply() *model.Post {
	return &model.Post{
		Id:        replyID,
		UserId:    replier,
		ChannelId: chanID,
		RootId:    echoID,
		Message:   "agreed!",
	}
}

// newMovedCopy is a redirect output: a reply already re-homed into the
// original thread, carrying the prop that marks it as moved.
func newMovedCopy() *model.Post {
	post := &model.Post{Id: movedID, UserId: replier, ChannelId: chanID, RootId: rootID, Message: "agreed!"}
	post.AddProp(ricRedirectedFromProp, replyID)
	return post
}

// testConfig returns a config with SiteURL set so buildPermalink works.
func testConfig() *model.Config {
	config := model.Config{}
	config.ServiceSettings.SiteURL = model.NewPointer(siteURL)
	return &config
}

func testChannel() *model.Channel {
	return &model.Channel{Id: chanID, TeamId: teamID, Name: "town-square", Type: model.ChannelTypeOpen}
}

func testTeam() *model.Team {
	return &model.Team{Id: teamID, Name: teamName}
}

// expectPermalinkEnv registers the lookups buildPermalink performs, which most
// redirect paths hit once they create the moved copy.
func expectPermalinkEnv(api *plugintest.API) {
	api.On("GetConfig").Return(testConfig())
	api.On("GetChannel", chanID).Return(testChannel(), nil)
	api.On("GetTeam", teamID).Return(testTeam(), nil)
}

func newTestPlugin(api *plugintest.API) *Plugin {
	p := &Plugin{}
	p.SetAPI(api)
	return p
}

// logMethods are the plugin API's logging methods, in level order.
var logMethods = []string{"LogDebug", "LogInfo", "LogWarn", "LogError"}

// allowLogging lets any plugin log call through the mock at any level.
// testify matches variadic mock calls against a fixed argument list, so a
// per-line expectation couples tests to each log line's arity; registering a
// range of arities once keeps tests blind to log-line shape.
func allowLogging(api *plugintest.API) {
	for _, method := range logMethods {
		for n := 1; n <= 12; n++ {
			args := make([]interface{}, n)
			for i := range args {
				args[i] = mock.Anything
			}
			api.On(method, args...).Maybe()
		}
	}
}

// assertLoggedLevel asserts the plugin logged at a given level (`method` must
// be one of logMethods) with a message containing substr. Inspecting recorded
// calls keeps the check arity-independent: level and message are asserted,
// key/value pairs are not.
func assertLoggedLevel(t *testing.T, api *plugintest.API, method, substr string) {
	t.Helper()
	if !slices.Contains(logMethods, method) {
		t.Fatalf("assertLoggedLevel: %q is not a logging method (want one of %v)", method, logMethods)
	}
	for _, call := range api.Calls {
		if call.Method != method {
			continue
		}
		if msg, ok := call.Arguments.Get(0).(string); ok && strings.Contains(msg, substr) {
			return
		}
	}
	t.Errorf("expected a %s call with message containing %q", method, substr)
}

func TestRedirectReplyToEchoPost(t *testing.T) {
	reply := newStrayReply()
	reply.FileIds = model.StringArray{fileID}
	reply.CreateAt = 111
	reply.UpdateAt = 222

	api := &plugintest.API{}
	api.On("GetPost", echoID).Return(newEchoPost(), nil)
	api.On("GetPost", rootID).Return(newOriginalRoot(), nil)
	api.On("CopyFileInfos", replier, []string{fileID}).Return([]string{copiedFileID}, nil)

	var moved *model.Post
	api.On("CreatePost", mock.Anything).
		Run(func(args mock.Arguments) { moved = args.Get(0).(*model.Post) }).
		Return(newMovedCopy(), nil)
	api.On("GetReactions", replyID).Return([]*model.Reaction{
		{UserId: reactor, PostId: replyID, EmojiName: "thumbsup", ChannelId: chanID},
	}, nil)
	var mirrored *model.Reaction
	api.On("AddReaction", mock.Anything).
		Run(func(args mock.Arguments) { mirrored = args.Get(0).(*model.Reaction) }).
		Return(nil, nil)
	api.On("DeletePost", replyID).Return(nil)
	expectPermalinkEnv(api)
	var ephemeral *model.Post
	api.On("SendEphemeralPost", replier, mock.Anything).
		Run(func(args mock.Arguments) { ephemeral = args.Get(1).(*model.Post) }).
		Return(nil)
	allowLogging(api)

	newTestPlugin(api).MessageHasBeenPosted(nil, reply)

	assert.NotNil(t, moved, "reply should be re-created in the original thread")
	assert.Equal(t, rootID, moved.RootId)
	assert.Equal(t, chanID, moved.ChannelId)
	assert.Equal(t, replier, moved.UserId)
	assert.Equal(t, "agreed!", moved.Message)
	// FileInfo rows can only be attached while unattached, so the moved copy
	// must get duplicated file infos, never the stray post's ids.
	assert.Equal(t, model.StringArray{copiedFileID}, moved.FileIds)
	assert.EqualValues(t, 111, moved.CreateAt, "original CreateAt must be preserved")
	assert.Equal(t, replyID, moved.GetProp(ricRedirectedFromProp), "moved copy records the stray post it replaces")

	assert.NotNil(t, mirrored, "reactions on the stray post are mirrored")
	assert.Equal(t, movedID, mirrored.PostId)
	assert.Equal(t, reactor, mirrored.UserId)

	api.AssertCalled(t, "DeletePost", replyID)

	assert.NotNil(t, ephemeral, "replier gets an ephemeral note")
	assert.Empty(t, ephemeral.RootId, "note is channel-level, not threaded")
	assert.Contains(t, ephemeral.Message, fmt.Sprintf("%s/%s/pl/%s", siteURL, teamName, movedID))

	// A moved copy is not redirected again, but it does flow through the normal
	// "view newer replies" logic against the echo post mapping.
	api2 := &plugintest.API{}
	api2.On("KVGet", kvKeyForThread(rootID)).Return([]byte(fmt.Sprintf("[%q]", echoID)), nil)
	api2.On("GetPost", echoID).Return(newEchoPost(), nil)
	api2.On("UpdatePost", mock.Anything).Return(nil, nil)
	expectPermalinkEnv(api2)
	allowLogging(api2)
	newTestPlugin(api2).MessageHasBeenPosted(nil, newMovedCopy())
	api2.AssertCalled(t, "UpdatePost", mock.Anything)
	api2.AssertNotCalled(t, "CreatePost", mock.Anything) // never re-redirected
}

func TestRedirectSkipsRegularThreadReplies(t *testing.T) {
	reply := newStrayReply()
	reply.RootId = rootID // a regular reply in the real thread
	reply.Message = "fine by me"

	api := &plugintest.API{}
	api.On("GetPost", rootID).Return(newOriginalRoot(), nil)
	api.On("KVGet", kvKeyForThread(rootID)).Return(nil, nil) // no /ric echo posts for this thread
	allowLogging(api)

	newTestPlugin(api).MessageHasBeenPosted(nil, reply)

	api.AssertNotCalled(t, "CreatePost", mock.Anything)
	api.AssertNotCalled(t, "DeletePost", mock.Anything)
	api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
}

func TestRedirectIgnoresAlreadyMovedCopy(t *testing.T) {
	api := &plugintest.API{}
	// The newer-replies lookup runs, but the redirect path is skipped entirely.
	api.On("KVGet", kvKeyForThread(rootID)).Return(nil, nil)
	allowLogging(api)

	newTestPlugin(api).MessageHasBeenPosted(nil, newMovedCopy())

	api.AssertNotCalled(t, "GetPost", mock.Anything)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestRedirectFailsOpenWhenCreateFails(t *testing.T) {
	api := &plugintest.API{}
	api.On("GetPost", echoID).Return(newEchoPost(), nil)
	api.On("GetPost", rootID).Return(newOriginalRoot(), nil)
	api.On("CreatePost", mock.Anything).Return(nil, model.NewAppError("CreatePost", "test_failure", nil, "", 500))
	allowLogging(api)

	newTestPlugin(api).MessageHasBeenPosted(nil, newStrayReply())

	// Fail open: the original reply stays in place, nothing is deleted.
	assertLoggedLevel(t, api, "LogWarn", "failed to copy reply")
	api.AssertNotCalled(t, "DeletePost", mock.Anything)
	api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
}

func TestRedirectLeavesReplyWhenOriginalRootDeleted(t *testing.T) {
	deletedRoot := newOriginalRoot()
	deletedRoot.DeleteAt = 999

	api := &plugintest.API{}
	api.On("GetPost", echoID).Return(newEchoPost(), nil)
	api.On("GetPost", rootID).Return(deletedRoot, nil)
	allowLogging(api)

	newTestPlugin(api).MessageHasBeenPosted(nil, newStrayReply())

	assertLoggedLevel(t, api, "LogWarn", "original thread root missing")
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
	api.AssertNotCalled(t, "DeletePost", mock.Anything)
}

func TestRedirectLeavesReplyOnSelfReferencingEchoPost(t *testing.T) {
	// Regression pin for the defensive branch in redirectReplyToOriginalThread:
	// an echo post whose ric_thread_root_id names itself cannot be produced by
	// ExecuteCommand, so if such data ever appears (manual DB edits, imported/
	// restored posts, future prop-semantics changes) the reply must be left
	// untouched instead of churned through a copy-delete cycle in its own thread.
	selfRef := newEchoPost()
	selfRef.AddProp(ricThreadRootIDProp, echoID) // points at itself

	api := &plugintest.API{}
	api.On("GetPost", echoID).Return(selfRef, nil)
	allowLogging(api)

	newTestPlugin(api).MessageHasBeenPosted(nil, newStrayReply())

	assertLoggedLevel(t, api, "LogWarn", "points at itself")
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
	api.AssertNotCalled(t, "DeletePost", mock.Anything)
	api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
}

func TestRedirectAbortsWhenFileCopyFails(t *testing.T) {
	reply := newStrayReply()
	reply.Message = "look"
	reply.FileIds = model.StringArray{fileID}

	api := &plugintest.API{}
	api.On("GetPost", echoID).Return(newEchoPost(), nil)
	api.On("GetPost", rootID).Return(newOriginalRoot(), nil)
	api.On("CopyFileInfos", replier, []string{fileID}).Return(nil, model.NewAppError("CopyFileInfos", "test_failure", nil, "", 500))
	allowLogging(api)

	newTestPlugin(api).MessageHasBeenPosted(nil, reply)

	// Nothing was committed when the file copy failed, so the redirect must
	// abort entirely: the reply keeps its attachments where it is instead of
	// moving as a lossy text-only copy.
	assertLoggedLevel(t, api, "LogWarn", "failed to copy attachments")
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
	api.AssertNotCalled(t, "DeletePost", mock.Anything)
	api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
}

func TestRicFromStrayThreadTargetsOriginalThread(t *testing.T) {
	// /ric issued inside a stray thread under an echo post must quote, reply
	// into, and map the ORIGINAL thread — not the stray one. Otherwise the
	// paired thread reply lands in the stray thread (and pre-fix, got
	// redirected with its attachments silently dropped).
	strayRoot := newEchoPost()
	strayRoot.Message = "echo text"
	realRoot := newOriginalRoot()
	realRoot.Message = "the real discussion"

	api := &plugintest.API{}
	api.On("GetPost", echoID).Return(strayRoot, nil)
	api.On("GetPost", rootID).Return(realRoot, nil)
	api.On("GetUser", rootAuthor).Return(&model.User{Id: rootAuthor, Username: rootAuthor}, nil)
	api.On("GetFileInfos", 0, 10, mock.Anything).Return([]*model.FileInfo{}, nil)

	var created []*model.Post
	callNum := 0
	api.On("CreatePost", mock.Anything).Return(
		func(post *model.Post) *model.Post {
			created = append(created, post.Clone())
			// Return distinct ids (ExecuteCommand chains them into KV
			// mappings and permalinks) without encoding creation order.
			callNum++
			out := post.Clone()
			out.Id = fmt.Sprintf("created%d", callNum)
			return out
		}, nil)
	api.On("KVGet", kvKeyForThread(rootID)).Return(nil, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	api.On("UpdatePost", mock.Anything).Return(nil, nil)
	expectPermalinkEnv(api)
	allowLogging(api)

	p := newTestPlugin(api)

	args := &model.CommandArgs{
		Command:   "/ric promoting from the stray thread",
		UserId:    replier,
		ChannelId: chanID,
		RootId:    echoID, // the stray thread the user typed in
	}
	resp, appErr := p.ExecuteCommand(nil, args)
	assert.Nil(t, appErr)
	assert.NotNil(t, resp)

	// Classify the two created posts by their actual shape, not by order.
	var channelPost, threadPost *model.Post
	for _, c := range created {
		if c.RootId == "" {
			channelPost = c
		} else {
			threadPost = c
		}
	}
	assert.NotNil(t, channelPost, "a channel-level echo post must be created")
	assert.NotNil(t, threadPost, "a paired thread reply must be created")
	assert.Equal(t, rootID, channelPost.GetProp(ricThreadRootIDProp), "echo post maps to the real thread root")
	assert.Contains(t, channelPost.Message, "@"+rootAuthor, "quote is attributed to the real thread's author")
	assert.Contains(t, channelPost.Message, "the real discussion", "quote shows the real root message")
	assert.Equal(t, rootID, threadPost.RootId, "paired thread reply lands in the real thread, not the stray one")

	api.AssertCalled(t, "KVSet", kvKeyForThread(rootID), mock.Anything)
}
