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
	assert.Equal(t, model.StringArray{"file1"}, moved.FileIds)
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
