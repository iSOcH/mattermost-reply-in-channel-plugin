package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// testConfig returns a config with SiteURL set so buildPermalink works.
func testConfig() *model.Config {
	config := model.Config{}
	config.ServiceSettings.SiteURL = model.NewPointer("http://localhost:8065")
	return &config
}

func testChannel() *model.Channel {
	return &model.Channel{Id: "chan1", TeamId: "team1", Name: "town-square", Type: model.ChannelTypeOpen}
}

// allowPermalinkDebug permits buildPermalink's single LogDebug call
// (msg + 2 key/value pairs) on mocks used in paths that build permalinks.
func allowPermalinkDebug(api *plugintest.API) {
	api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
}

func TestRedirectReplyToEchoPost(t *testing.T) {
	echoPost := &model.Post{Id: "echo1", UserId: "alice", ChannelId: "chan1"}
	echoPost.AddProp(ricThreadRootIDProp, "root1")
	origRoot := &model.Post{Id: "root1", UserId: "bob", ChannelId: "chan1"}
	reply := &model.Post{
		Id:        "reply1",
		UserId:    "carol",
		ChannelId: "chan1",
		RootId:    "echo1",
		Message:   "agreed!",
		FileIds:   model.StringArray{"file1"},
		CreateAt:  111,
		UpdateAt:  222,
	}

	api := &plugintest.API{}
	api.On("GetPost", "echo1").Return(echoPost, nil)
	api.On("GetPost", "root1").Return(origRoot, nil)
	api.On("CopyFileInfos", "carol", []string{"file1"}).Return([]string{"file1-copy"}, nil)

	var moved *model.Post
	api.On("CreatePost", mock.Anything).
		Run(func(args mock.Arguments) { moved = args.Get(0).(*model.Post) }).
		Return(&model.Post{Id: "moved1", UserId: "carol", ChannelId: "chan1", RootId: "root1"}, nil)
	api.On("GetReactions", "reply1").Return([]*model.Reaction{
		{UserId: "dave", PostId: "reply1", EmojiName: "thumbsup", ChannelId: "chan1"},
	}, nil)
	var mirrored *model.Reaction
	api.On("AddReaction", mock.Anything).
		Run(func(args mock.Arguments) { mirrored = args.Get(0).(*model.Reaction) }).
		Return(nil, nil)
	api.On("DeletePost", "reply1").Return(nil)
	api.On("GetConfig").Return(testConfig())
	api.On("GetChannel", "chan1").Return(testChannel(), nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "myteam"}, nil)
	var ephemeral *model.Post
	api.On("SendEphemeralPost", "carol", mock.Anything).
		Run(func(args mock.Arguments) { ephemeral = args.Get(1).(*model.Post) }).
		Return(nil)
	allowPermalinkDebug(api)

	p := &Plugin{}
	p.SetAPI(api)
	p.MessageHasBeenPosted(nil, reply)

	assert.NotNil(t, moved, "reply should be re-created in the original thread")
	assert.Equal(t, "root1", moved.RootId)
	assert.Equal(t, "chan1", moved.ChannelId)
	assert.Equal(t, "carol", moved.UserId)
	assert.Equal(t, "agreed!", moved.Message)
	// FileInfo rows can only be attached while unattached, so the moved copy
	// must get duplicated file infos, never the stray post's ids.
	assert.Equal(t, model.StringArray{"file1-copy"}, moved.FileIds)
	assert.EqualValues(t, 111, moved.CreateAt, "original CreateAt must be preserved")
	assert.Equal(t, "reply1", moved.GetProp(ricRedirectedFromProp), "moved copy records the stray post it replaces")

	assert.NotNil(t, mirrored, "reactions on the stray post are mirrored")
	assert.Equal(t, "moved1", mirrored.PostId)
	assert.Equal(t, "dave", mirrored.UserId)

	api.AssertCalled(t, "DeletePost", "reply1")

	assert.NotNil(t, ephemeral, "replier gets an ephemeral note")
	assert.Empty(t, ephemeral.RootId, "note is channel-level, not threaded")
	assert.Contains(t, ephemeral.Message, "http://localhost:8065/myteam/pl/moved1")

	// A moved copy is not redirected again, but it does flow through the normal
	// "view newer replies" logic against the echo post mapping.
	api2 := &plugintest.API{}
	api2.On("KVGet", kvKeyForThread("root1")).Return([]byte(`["echo1"]`), nil)
	api2.On("GetPost", "echo1").Return(echoPost, nil)
	api2.On("UpdatePost", mock.Anything).Return(nil, nil)
	api2.On("GetConfig").Return(testConfig())
	api2.On("GetChannel", "chan1").Return(testChannel(), nil)
	api2.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "myteam"}, nil)
	allowPermalinkDebug(api2)
	createdCopy := &model.Post{Id: "moved1", UserId: "carol", ChannelId: "chan1", RootId: "root1", Message: "agreed!"}
	createdCopy.AddProp(ricRedirectedFromProp, "reply1")
	p2 := &Plugin{}
	p2.SetAPI(api2)
	p2.MessageHasBeenPosted(nil, createdCopy)
	api2.AssertCalled(t, "UpdatePost", mock.Anything)
	api2.AssertNotCalled(t, "CreatePost", mock.Anything) // never re-redirected
}

func TestRedirectSkipsRegularThreadReplies(t *testing.T) {
	regularRoot := &model.Post{Id: "root1", UserId: "bob", ChannelId: "chan1"}
	reply := &model.Post{Id: "reply1", UserId: "carol", ChannelId: "chan1", RootId: "root1", Message: "fine by me"}

	api := &plugintest.API{}
	api.On("GetPost", "root1").Return(regularRoot, nil)
	api.On("KVGet", kvKeyForThread("root1")).Return(nil, nil) // no /ric echo posts for this thread

	p := &Plugin{}
	p.SetAPI(api)
	p.MessageHasBeenPosted(nil, reply)

	api.AssertNotCalled(t, "CreatePost", mock.Anything)
	api.AssertNotCalled(t, "DeletePost", mock.Anything)
	api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
}

func TestRedirectIgnoresAlreadyMovedCopy(t *testing.T) {
	moved := &model.Post{Id: "moved1", UserId: "carol", ChannelId: "chan1", RootId: "root1", Message: "agreed!"}
	moved.AddProp(ricRedirectedFromProp, "reply1")

	api := &plugintest.API{}
	// The newer-replies lookup runs, but the redirect path is skipped entirely.
	api.On("KVGet", kvKeyForThread("root1")).Return(nil, nil)

	p := &Plugin{}
	p.SetAPI(api)
	p.MessageHasBeenPosted(nil, moved)

	api.AssertNotCalled(t, "GetPost", mock.Anything)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
}

func TestRedirectFailsOpenWhenCreateFails(t *testing.T) {
	echoPost := &model.Post{Id: "echo1", UserId: "alice", ChannelId: "chan1"}
	echoPost.AddProp(ricThreadRootIDProp, "root1")
	origRoot := &model.Post{Id: "root1", UserId: "bob", ChannelId: "chan1"}
	reply := &model.Post{Id: "reply1", UserId: "carol", ChannelId: "chan1", RootId: "echo1", Message: "agreed!"}

	api := &plugintest.API{}
	api.On("GetPost", "echo1").Return(echoPost, nil)
	api.On("GetPost", "root1").Return(origRoot, nil)
	api.On("CreatePost", mock.Anything).Return(nil, model.NewAppError("CreatePost", "test_failure", nil, "", 500))
	// LogWarn with the arity used on this path: msg + 6 keys/values
	api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)

	p := &Plugin{}
	p.SetAPI(api)
	p.MessageHasBeenPosted(nil, reply)

	// Fail open: the original reply stays in place, nothing is deleted.
	api.AssertNotCalled(t, "DeletePost", mock.Anything)
	api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
}

func TestRedirectLeavesReplyWhenOriginalRootDeleted(t *testing.T) {
	echoPost := &model.Post{Id: "echo1", UserId: "alice", ChannelId: "chan1"}
	echoPost.AddProp(ricThreadRootIDProp, "root1")
	deletedRoot := &model.Post{Id: "root1", UserId: "bob", ChannelId: "chan1", DeleteAt: 999}
	reply := &model.Post{Id: "reply1", UserId: "carol", ChannelId: "chan1", RootId: "echo1", Message: "agreed!"}

	api := &plugintest.API{}
	api.On("GetPost", "echo1").Return(echoPost, nil)
	api.On("GetPost", "root1").Return(deletedRoot, nil)
	// LogWarn with the arity used on this path: msg + 4 keys/values
	api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)

	p := &Plugin{}
	p.SetAPI(api)
	p.MessageHasBeenPosted(nil, reply)

	api.AssertNotCalled(t, "CreatePost", mock.Anything)
	api.AssertNotCalled(t, "DeletePost", mock.Anything)
}

func TestRedirectLeavesReplyOnSelfReferencingEchoPost(t *testing.T) {
	// Regression pin for the defensive branch in redirectReplyToOriginalThread:
	// an echo post whose ric_thread_root_id names itself cannot be produced by
	// ExecuteCommand, so if such data ever appears (manual DB edits, imported/
	// restored posts, future prop-semantics changes) the reply must be left
	// untouched instead of churned through a copy-delete cycle in its own thread.
	selfRef := &model.Post{Id: "echo1", UserId: "alice", ChannelId: "chan1"}
	selfRef.AddProp(ricThreadRootIDProp, "echo1")
	reply := &model.Post{Id: "reply1", UserId: "carol", ChannelId: "chan1", RootId: "echo1", Message: "agreed!"}

	api := &plugintest.API{}
	api.On("GetPost", "echo1").Return(selfRef, nil)
	// LogWarn with the arity used on this path: msg + 4 keys/values
	api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)

	p := &Plugin{}
	p.SetAPI(api)
	p.MessageHasBeenPosted(nil, reply)

	api.AssertCalled(t, "LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	api.AssertNotCalled(t, "CreatePost", mock.Anything)
	api.AssertNotCalled(t, "DeletePost", mock.Anything)
	api.AssertNotCalled(t, "SendEphemeralPost", mock.Anything, mock.Anything)
}

func TestRedirectStaysTextOnlyWhenFileCopyFails(t *testing.T) {
	echoPost := &model.Post{Id: "echo1", UserId: "alice", ChannelId: "chan1"}
	echoPost.AddProp(ricThreadRootIDProp, "root1")
	origRoot := &model.Post{Id: "root1", UserId: "bob", ChannelId: "chan1"}
	reply := &model.Post{Id: "reply1", UserId: "carol", ChannelId: "chan1", RootId: "echo1", Message: "look", FileIds: model.StringArray{"file1"}}

	api := &plugintest.API{}
	api.On("GetPost", "echo1").Return(echoPost, nil)
	api.On("GetPost", "root1").Return(origRoot, nil)
	api.On("CopyFileInfos", "carol", []string{"file1"}).Return(nil, model.NewAppError("CopyFileInfos", "test_failure", nil, "", 500))
	// LogWarn with the arity used on the copy-failure path: msg + 4 keys/values
	api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	var moved *model.Post
	api.On("CreatePost", mock.Anything).
		Run(func(args mock.Arguments) { moved = args.Get(0).(*model.Post) }).
		Return(&model.Post{Id: "moved1", UserId: "carol", ChannelId: "chan1", RootId: "root1"}, nil)
	api.On("GetReactions", "reply1").Return([]*model.Reaction{}, nil)
	api.On("DeletePost", "reply1").Return(nil)
	api.On("GetConfig").Return(testConfig())
	api.On("GetChannel", "chan1").Return(testChannel(), nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "myteam"}, nil)
	api.On("SendEphemeralPost", "carol", mock.Anything).Return(nil)
	allowPermalinkDebug(api)

	p := &Plugin{}
	p.SetAPI(api)
	p.MessageHasBeenPosted(nil, reply)

	// Fail open: the reply text still moves, just without attachments.
	assert.NotNil(t, moved)
	assert.Empty(t, moved.FileIds)
	assert.Equal(t, "look", moved.Message)
	api.AssertCalled(t, "DeletePost", "reply1")
}

func TestRicFromStrayThreadTargetsOriginalThread(t *testing.T) {
	// /ric issued inside a stray thread under an echo post must quote, reply
	// into, and map the ORIGINAL thread — not the stray one. Otherwise the
	// paired thread reply lands in the stray thread (and pre-fix, got
	// redirected with its attachments silently dropped).
	strayRoot := &model.Post{Id: "echo1", UserId: "alice", ChannelId: "chan1", Message: "echo text"}
	strayRoot.AddProp(ricThreadRootIDProp, "root1")
	realRoot := &model.Post{Id: "root1", UserId: "bob", ChannelId: "chan1", Message: "the real discussion"}

	api := &plugintest.API{}
	api.On("GetPost", "echo1").Return(strayRoot, nil)
	api.On("GetPost", "root1").Return(realRoot, nil)
	api.On("GetUser", "bob").Return(&model.User{Id: "bob", Username: "bob"}, nil)
	api.On("GetFileInfos", 0, 10, mock.Anything).Return([]*model.FileInfo{}, nil)

	var created []*model.Post
	callNum := 0
	api.On("CreatePost", mock.Anything).Return(
		func(post *model.Post) *model.Post {
			callNum++
			created = append(created, post)
			out := post.Clone()
			if callNum == 1 {
				out.Id = "echo2"
			} else {
				out.Id = "tp1"
			}
			return out
		}, nil)
	api.On("KVGet", kvKeyForThread("root1")).Return(nil, nil)
	api.On("KVSet", mock.Anything, mock.Anything).Return(nil)
	api.On("UpdatePost", mock.Anything).Return(nil, nil)
	api.On("GetConfig").Return(testConfig())
	api.On("GetChannel", "chan1").Return(testChannel(), nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "myteam"}, nil)
	api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
	api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	p := &Plugin{}
	p.SetAPI(api)

	args := &model.CommandArgs{
		Command:   "/ric promoting from the stray thread",
		UserId:    "carol",
		ChannelId: "chan1",
		RootId:    "echo1", // the stray thread the user typed in
	}
	resp, appErr := p.ExecuteCommand(nil, args)
	assert.Nil(t, appErr)
	assert.NotNil(t, resp)

	assert.Equal(t, 2, len(created), "one channel post and one thread reply")
	channelPost, threadPost := created[0], created[1]
	assert.Empty(t, channelPost.RootId)
	assert.Equal(t, "root1", channelPost.GetProp(ricThreadRootIDProp), "echo post maps to the real thread root")
	assert.Contains(t, channelPost.Message, "@bob", "quote is attributed to the real thread's author")
	assert.Contains(t, channelPost.Message, "the real discussion", "quote shows the real root message")
	assert.Equal(t, "root1", threadPost.RootId, "paired thread reply lands in the real thread, not the stray one")

	api.AssertCalled(t, "KVSet", kvKeyForThread("root1"), mock.Anything)
}
