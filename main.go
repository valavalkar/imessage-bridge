// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	flag "maunium.net/go/mauflag"
	"maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/bridge/bridgeconfig"

	"maunium.net/go/mautrix/event"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/commands"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/util/configupgrade"

	"go.mau.fi/mautrix-imessage/config"
	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
	_ "go.mau.fi/mautrix-imessage/imessage/ios"
	_ "go.mau.fi/mautrix-imessage/imessage/mac-nosip"
	"go.mau.fi/mautrix-imessage/ipc"
)

var (
	// These are filled at build time with the -X linker flag
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

//go:embed example-config.yaml
var ExampleConfig string

var configURL = flag.MakeFull("u", "url", "The URL to download the config file from.", "").String()
var configOutputRedirect = flag.MakeFull("o", "output-redirect", "Whether or not to output the URL of the first redirect when downloading the config file.", "false").Bool()
var checkPermissions = flag.MakeFull("p", "check-permissions", "Check for full disk access permissions and quit.", "false").Bool()

type IMBridge struct {
	bridge.Bridge
	Config    *config.Config
	DB        *database.Database
	IM        imessage.API
	IMHandler *iMessageHandler
	IPC       *ipc.Processor

	WebsocketHandler *WebsocketCommandHandler

	user          *User
	portalsByMXID map[id.RoomID]*Portal
	portalsByGUID map[string]*Portal
	portalsLock   sync.Mutex
	userCache     map[id.UserID]*User
	puppets       map[string]*Puppet
	puppetsLock   sync.Mutex
	stopping      bool
	stop          chan struct{}
	stopPinger    chan struct{}
	latestState   *imessage.BridgeStatus
	pushKey       *imessage.PushKeyRequest

	shortCircuitReconnectBackoff chan struct{}
	websocketStarted             chan struct{}
	websocketStopped             chan struct{}

	SendStatusStartTS    int64
	sendStatusUpdateInfo bool
	wasConnected         bool
	hackyTestLoopStarted bool

	pendingHackyTestGUID     string
	pendingHackyTestRandomID string
	hackyTestSuccess         bool
}

func (br *IMBridge) GetExampleConfig() string {
	return ExampleConfig
}

func (br *IMBridge) GetConfigPtr() interface{} {
	br.Config = &config.Config{
		BaseConfig: &br.Bridge.Config,
	}
	br.Config.BaseConfig.Bridge = &br.Config.Bridge
	return br.Config
}

func (br *IMBridge) GetIPortal(roomID id.RoomID) bridge.Portal {
	portal := br.GetPortalByMXID(roomID)
	if portal != nil {
		return portal
	}
	return nil
}

func (br *IMBridge) GetAllIPortals() (iportals []bridge.Portal) {
	portals := br.GetAllPortals()
	iportals = make([]bridge.Portal, len(portals))
	for i, portal := range portals {
		iportals[i] = portal
	}
	return iportals
}

func (br *IMBridge) GetIUser(id id.UserID, create bool) bridge.User {
	if id == br.user.MXID {
		return br.user
	}
	cached, ok := br.userCache[id]
	if !ok {
		if !create {
			return nil
		}
		cached = &User{
			User:   &database.User{MXID: id},
			bridge: br,
			log:    br.Log.Sub("ExtUser").Sub(id.String()),
		}
		br.userCache[id] = cached
	}
	return cached
}

func (br *IMBridge) IsGhost(userID id.UserID) bool {
	_, isPuppet := br.ParsePuppetMXID(userID)
	return isPuppet
}

func (br *IMBridge) GetIGhost(userID id.UserID) bridge.Ghost {
	puppet := br.GetPuppetByMXID(userID)
	if puppet != nil {
		return puppet
	}
	return nil
}

func (br *IMBridge) CreatePrivatePortal(roomID id.RoomID, user bridge.User, ghost bridge.Ghost) {
	// TODO implement
}

func (br *IMBridge) ensureConnection() {
	for {
		resp, err := br.Bot.Whoami()
		if err != nil {
			if httpErr, ok := err.(mautrix.HTTPError); ok && httpErr.RespError != nil && httpErr.RespError.ErrCode == "M_UNKNOWN_ACCESS_TOKEN" {
				br.Log.Fatalln("Access token invalid. Is the registration installed in your homeserver correctly?")
				os.Exit(16)
			}
			br.Log.Errorfln("Failed to connect to homeserver: %v. Retrying in 10 seconds...", err)
			time.Sleep(10 * time.Second)
		} else if resp.UserID != br.Bot.UserID {
			br.Log.Fatalln("Unexpected user ID in whoami call: got %s, expected %s", resp.UserID, br.Bot.UserID)
			os.Exit(17)
		} else {
			break
		}
	}
}

func (br *IMBridge) Init() {
	br.CommandProcessor = commands.NewProcessor(&br.Bridge)
	br.DB = database.New(br.Bridge.DB, br.Log.Sub("Database"))

	br.initSegment()

	br.IPC = ipc.NewStdioProcessor(br.Log, br.Config.IMessage.LogIPCPayloads)
	br.IPC.SetHandler("reset-encryption", br.ipcResetEncryption)
	br.IPC.SetHandler("ping", br.ipcPing)
	br.IPC.SetHandler("ping-server", br.ipcPingServer)
	br.IPC.SetHandler("stop", br.ipcStop)
	br.IPC.SetHandler("merge-rooms", br.ipcMergeRooms)
	br.IPC.SetHandler("split-rooms", br.ipcSplitRooms)
	br.IPC.SetHandler("do-auto-merge", br.ipcDoAutoMerge)

	br.Log.Debugln("Initializing iMessage connector")
	var err error
	br.IM, err = imessage.NewAPI(br)
	if err != nil {
		br.Log.Fatalln("Failed to initialize iMessage connector:", err)
		os.Exit(14)
	}

	if br.Config.IMessage.Platform == "android" {
		br.EventProcessor.PrependHandler(event.EventEncrypted, func(evt *event.Event) {
			go br.IM.NotifyUpcomingMessage(evt.ID)
		})
		br.Bridge.BeeperNetworkName = "androidsms"
		br.Bridge.BeeperServiceName = "androidsms"
	} else if br.Config.IMessage.Platform == "mac-nosip" {
		br.Bridge.BeeperNetworkName = "imessage"
		br.Bridge.BeeperServiceName = "imessagecloud"
	} else {
		br.Bridge.BeeperNetworkName = "imessage"
		br.Bridge.BeeperServiceName = "imessage"
	}

	br.IMHandler = NewiMessageHandler(br)
	br.WebsocketHandler = NewWebsocketCommandHandler(br)
}

type PingResponse struct {
	OK bool `json:"ok"`
}

func (br *IMBridge) GetIPC() *ipc.Processor {
	return br.IPC
}

func (br *IMBridge) GetLog() maulogger.Logger {
	return br.Log
}

func (br *IMBridge) GetConnectorConfig() *imessage.PlatformConfig {
	return &br.Config.IMessage
}

type PingData struct {
	Timestamp int64 `json:"timestamp"`
}

func (br *IMBridge) PingServer() (start, serverTs, end time.Time) {
	if !br.AS.HasWebsocket() {
		br.Log.Debugln("Received server ping request, but no websocket connected. Trying to short-circuit backoff sleep")
		select {
		case br.shortCircuitReconnectBackoff <- struct{}{}:
		default:
			br.Log.Warnfln("Failed to ping websocket: not connected and no backoff?")
			return
		}
		select {
		case <-br.websocketStarted:
		case <-time.After(15 * time.Second):
			if !br.AS.HasWebsocket() {
				br.Log.Warnfln("Failed to ping websocket: didn't connect after 15 seconds of waiting")
				return
			}
		}
	}
	start = time.Now()
	var resp PingData
	br.Log.Debugln("Pinging appservice websocket")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := br.AS.RequestWebsocket(ctx, &appservice.WebsocketRequest{
		Command: "ping",
		Data:    &PingData{Timestamp: start.UnixMilli()},
	}, &resp)
	end = time.Now()
	if err != nil {
		br.Log.Warnfln("Websocket ping returned error in %s: %v", end.Sub(start), err)
		br.AS.StopWebsocket(fmt.Errorf("websocket ping returned error in %s: %w", end.Sub(start), err))
	} else {
		serverTs = time.Unix(0, resp.Timestamp*int64(time.Millisecond))
		br.Log.Debugfln("Websocket ping returned success in %s (request: %s, response: %s)", end.Sub(start), serverTs.Sub(start), end.Sub(serverTs))
	}
	return
}

func (br *IMBridge) ipcResetEncryption(_ json.RawMessage) interface{} {
	br.Crypto.Reset(true)
	return PingResponse{true}
}

func (br *IMBridge) ipcPing(_ json.RawMessage) interface{} {
	return PingResponse{true}
}

type PingServerResponse struct {
	Start  int64 `json:"start_ts"`
	Server int64 `json:"server_ts"`
	End    int64 `json:"end_ts"`
}

func (br *IMBridge) ipcPingServer(_ json.RawMessage) interface{} {
	start, server, end := br.PingServer()
	return &PingServerResponse{
		Start:  start.UnixNano(),
		Server: server.UnixNano(),
		End:    end.UnixNano(),
	}
}

type ipcMergeRequest struct {
	GUIDs []string `json:"guids"`
}

type ipcMergeResponse struct {
	MXID id.RoomID `json:"mxid"`
}

func (br *IMBridge) ipcMergeRooms(rawReq json.RawMessage) interface{} {
	var req ipcMergeRequest
	err := json.Unmarshal(rawReq, &req)
	if err != nil {
		return err
	}
	var portals []*Portal
	for _, guid := range req.GUIDs {
		portals = append(portals, br.GetPortalByGUID(guid))
	}
	if len(portals) < 2 {
		return fmt.Errorf("must pass at least 2 portals to merge")
	}
	portals[0].Merge(portals[1:])
	return ipcMergeResponse{MXID: portals[0].MXID}
}

type ipcSplitRequest struct {
	GUID  string              `json:"guid"`
	Parts map[string][]string `json:"parts"`
}

type ipcSplitResponse struct{}

func (br *IMBridge) ipcSplitRooms(rawReq json.RawMessage) interface{} {
	var req ipcSplitRequest
	err := json.Unmarshal(rawReq, &req)
	if err != nil {
		return err
	}
	sourcePortal := br.GetPortalByGUID(req.GUID)
	sourcePortal.Split(req.Parts)
	return ipcSplitResponse{}
}

func (br *IMBridge) ipcDoAutoMerge(_ json.RawMessage) any {
	contacts, err := br.IM.GetContactList()
	if err != nil {
		return fmt.Errorf("failed to get contact list: %w", err)
	}
	br.UpdateMerges(contacts)
	return struct{}{}
}

const defaultReconnectBackoff = 2 * time.Second
const maxReconnectBackoff = 2 * time.Minute
const reconnectBackoffReset = 5 * time.Minute

type StartSyncRequest struct {
	AccessToken string      `json:"access_token"`
	DeviceID    id.DeviceID `json:"device_id"`
	UserID      id.UserID   `json:"user_id"`
}

const BridgeStatusConnected = "CONNECTED"

func (br *IMBridge) SendBridgeStatus(state imessage.BridgeStatus) {
	br.Log.Debugfln("Sending bridge status to server: %+v", state)
	if state.Timestamp == 0 {
		state.Timestamp = time.Now().Unix()
	}
	if state.TTL == 0 {
		state.TTL = 600
	}
	if len(state.Source) == 0 {
		state.Source = "bridge"
	}
	if len(state.UserID) == 0 {
		state.UserID = br.user.MXID
	}
	if br.IM.Capabilities().BridgeState {
		br.latestState = &state
	}
	err := br.AS.SendWebsocket(&appservice.WebsocketRequest{
		Command: "bridge_status",
		Data:    &state,
	})
	if err != nil {
		br.Log.Warnln("Error sending bridge status:", err)
	}
	if br.Config.HackyStartupTest.Identifier != "" && state.StateEvent == BridgeStatusConnected && !br.Config.HackyStartupTest.EchoMode {
		br.wasConnected = true
		if !br.wasConnected {
			go br.hackyStartupTests(true, false)
		}
		if !br.hackyTestLoopStarted && br.Config.HackyStartupTest.PeriodicResolve > 0 {
			br.hackyTestLoopStarted = true
			go br.hackyTestLoop()
		}
	}
}

func (br *IMBridge) sendPushKey() {
	if br.pushKey == nil {
		return
	}
	err := br.AS.RequestWebsocket(context.Background(), &appservice.WebsocketRequest{
		Command: "push_key",
		Data:    br.pushKey,
	}, nil)
	if err != nil {
		// Don't care about websocket not connected errors, we'll retry automatically when reconnecting
		if !errors.Is(err, appservice.ErrWebsocketNotConnected) {
			br.Log.Warnln("Error sending push key to asmux:", err)
		}
	} else {
		br.Log.Infoln("Successfully sent push key to asmux")
	}
}

func (br *IMBridge) SetPushKey(req *imessage.PushKeyRequest) {
	if req.PushKeyTS == 0 {
		req.PushKeyTS = time.Now().Unix()
	}
	br.pushKey = req
	go br.sendPushKey()
}

func (br *IMBridge) RequestStartSync() {
	if !br.Config.Bridge.Encryption.Appservice ||
		br.Config.Homeserver.Software == bridgeconfig.SoftwareHungry ||
		br.Crypto == nil ||
		!br.AS.HasWebsocket() {
		return
	}
	resp := map[string]interface{}{}
	br.Log.Debugln("Sending /sync start request through websocket")
	cryptoClient := br.Crypto.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	err := br.AS.RequestWebsocket(ctx, &appservice.WebsocketRequest{
		Command:  "start_sync",
		Deadline: 30 * time.Second,
		Data: &StartSyncRequest{
			AccessToken: cryptoClient.AccessToken,
			DeviceID:    cryptoClient.DeviceID,
			UserID:      cryptoClient.UserID,
		},
	}, &resp)
	if err != nil {
		go br.WebsocketHandler.HandleSyncProxyError(nil, err)
	} else {
		br.Log.Debugln("Started receiving encryption data with sync proxy:", resp)
	}
}

func (br *IMBridge) startWebsocket(wg *sync.WaitGroup) {
	var wgOnce sync.Once
	onConnect := func() {
		if br.latestState != nil {
			go br.SendBridgeStatus(*br.latestState)
		} else if !br.IM.Capabilities().BridgeState {
			go br.SendBridgeStatus(imessage.BridgeStatus{
				StateEvent: BridgeStatusConnected,
				RemoteID:   "unknown",
			})
		}
		go br.sendPushKey()
		br.RequestStartSync()
		wgOnce.Do(wg.Done)
		select {
		case br.websocketStarted <- struct{}{}:
		default:
		}
	}
	reconnectBackoff := defaultReconnectBackoff
	lastDisconnect := time.Now().UnixNano()
	defer func() {
		br.Log.Debugfln("Appservice websocket loop finished")
		close(br.websocketStopped)
	}()
	for {
		err := br.AS.StartWebsocket(br.Config.Homeserver.WSProxy, onConnect)
		if err == appservice.ErrWebsocketManualStop {
			return
		} else if closeCommand := (&appservice.CloseCommand{}); errors.As(err, &closeCommand) && closeCommand.Status == appservice.MeowConnectionReplaced {
			br.Log.Infoln("Appservice websocket closed by another instance of the bridge, shutting down...")
			br.Stop()
			return
		} else if err != nil {
			br.Log.Errorln("Error in appservice websocket:", err)
		}
		if br.stopping {
			return
		}
		now := time.Now().UnixNano()
		if lastDisconnect+reconnectBackoffReset.Nanoseconds() < now {
			reconnectBackoff = defaultReconnectBackoff
		} else {
			reconnectBackoff *= 2
			if reconnectBackoff > maxReconnectBackoff {
				reconnectBackoff = maxReconnectBackoff
			}
		}
		lastDisconnect = now
		br.Log.Infofln("Websocket disconnected, reconnecting in %d seconds...", int(reconnectBackoff.Seconds()))
		select {
		case <-br.shortCircuitReconnectBackoff:
			br.Log.Debugln("Reconnect backoff was short-circuited")
		case <-time.After(reconnectBackoff):
		}
		if br.stopping {
			return
		}
	}
}

func (br *IMBridge) connectToiMessage(wg *sync.WaitGroup) {
	err := br.IM.Start(wg.Done)
	if err != nil {
		br.Log.Fatalln("Error in iMessage connection:", err)
		os.Exit(40)
	}
}

func (br *IMBridge) Start() {
	if br.Config.Bridge.MessageStatusEvents {
		sendStatusStart := br.DB.KV.Get(database.KVSendStatusStart)
		if len(sendStatusStart) > 0 {
			br.SendStatusStartTS, _ = strconv.ParseInt(sendStatusStart, 10, 64)
		}
		if br.SendStatusStartTS == 0 {
			br.SendStatusStartTS = time.Now().UnixMilli()
			br.DB.KV.Set(database.KVSendStatusStart, strconv.FormatInt(br.SendStatusStartTS, 10))
			br.sendStatusUpdateInfo = true
		}
	}
	br.wasConnected = br.DB.KV.Get(database.KVBridgeWasConnected) == "true"

	needsPortalFinding := br.Config.Bridge.FindPortalsIfEmpty && br.DB.Portal.Count() == 0

	br.Log.Debugln("Finding bridge user")
	br.user = br.loadDBUser()
	br.user.initDoublePuppet()
	var startupGroup sync.WaitGroup
	startupGroup.Add(2)
	br.Log.Debugln("Connecting to iMessage")
	go br.connectToiMessage(&startupGroup)

	if needsPortalFinding {
		br.Log.Infoln("Portal database is empty, finding portals from Matrix room state")
		err := br.FindPortalsFromMatrix()
		if err != nil {
			br.Log.Fatalln("Error finding portals:", err)
			os.Exit(30)
		}
		// The database was probably reset, so log out of all bridge bot devices to keep the list clean
		if br.Crypto != nil {
			br.Crypto.Reset(true)
		}
	}

	if br.Config.Homeserver.WSProxy != "" {
		br.Log.Debugln("Starting application service websocket")
		go br.startWebsocket(&startupGroup)
	} else {
		if br.Config.AppService.Port == 0 {
			br.Log.Fatalln("Both the websocket proxy and appservice listener are disabled, can't receive events")
			os.Exit(23)
		}
		br.Log.Debugln("Websocket proxy not configured, not starting application service websocket")
	}

	br.Log.Debugln("Starting iMessage handler")
	go br.IMHandler.Start()
	startupGroup.Wait()
	br.Log.Debugln("Starting IPC loop")
	go br.IPC.Loop()

	go br.StartupSync()
	br.Log.Infoln("Initialization complete")
	go br.PeriodicSync()

	br.stopPinger = make(chan struct{})
	if br.Config.Homeserver.WSPingInterval > 0 {
		go br.serverPinger()
	}
}

func (br *IMBridge) serverPinger() {
	interval := time.Duration(br.Config.Homeserver.WSPingInterval) * time.Second
	clock := time.NewTicker(interval)
	defer func() {
		br.Log.Infofln("Websocket pinger stopped")
		clock.Stop()
	}()
	br.Log.Infofln("Pinging websocket every %s", interval)
	for {
		select {
		case <-clock.C:
			br.PingServer()
		case <-br.stopPinger:
			return
		}
		if br.stopping {
			return
		}
	}
}

func (br *IMBridge) StartupSync() {
	resp, err := br.IM.PreStartupSyncHook()
	if err != nil {
		br.Log.Errorln("iMessage connector returned error in startup sync hook:", err)
	} else if resp.SkipSync {
		br.Log.Debugln("Skipping startup sync")
		return
	}

	forceUpdateBridgeInfo := br.sendStatusUpdateInfo ||
		br.DB.KV.Get(database.KVBridgeInfoVersion) != database.ExpectedBridgeInfoVersion
	alreadySynced := make(map[string]bool)
	for _, portal := range br.GetAllPortals() {
		removed := portal.CleanupIfEmpty(true)
		if !removed && len(portal.MXID) > 0 {
			if br.Config.Bridge.DisableSMSPortals && portal.Identifier.Service == "SMS" && !portal.Identifier.IsGroup {
				imIdentifier := portal.Identifier
				imIdentifier.Service = "iMessage"
				if !portal.reIDInto(imIdentifier.String(), true, true) {
					// Portal was dropped/merged, don't sync it
					continue
				} // else: portal was re-id'd, sync it as usual
			} else if !br.Config.Bridge.DisableSMSPortals && portal.Identifier.Service == "iMessage" && !portal.Identifier.IsGroup && portal.LastSeenHandle != "" {
				lastSeenHandle := imessage.ParseIdentifier(portal.LastSeenHandle)
				if lastSeenHandle.Service == "SMS" && lastSeenHandle.LocalID == portal.Identifier.LocalID {
					if !portal.reIDInto(portal.LastSeenHandle, true, true) {
						continue
					}
				}
			}
			portal.log.Infoln("Syncing portal (startup sync, existing portal)")
			portal.Sync(true)
			alreadySynced[portal.GUID] = true
			if forceUpdateBridgeInfo {
				portal.UpdateBridgeInfo()
			}
		}
	}
	if forceUpdateBridgeInfo {
		br.DB.KV.Set(database.KVBridgeInfoVersion, database.ExpectedBridgeInfoVersion)
	}
	syncChatMaxAge := time.Duration(br.Config.Bridge.Backfill.InitialSyncMaxAge*24*60) * time.Minute
	chats, err := br.IM.GetChatsWithMessagesAfter(time.Now().Add(-syncChatMaxAge))
	if err != nil {
		br.Log.Errorln("Failed to get chat list to backfill:", err)
		return
	}
	for _, chat := range chats {
		if _, isSynced := alreadySynced[chat.ChatGUID]; !isSynced {
			portal := br.GetPortalByGUID(chat.ChatGUID)
			if portal.ThreadID == "" {
				portal.ThreadID = chat.ThreadID
			}
			portal.log.Infoln("Syncing portal (startup sync, new portal)")
			portal.Sync(true)
		}
	}
	br.Log.Infoln("Startup sync complete")
	br.IM.PostStartupSyncHook()
}

func (br *IMBridge) PeriodicSync() {
	if !br.Config.Bridge.PeriodicSync {
		br.Log.Debugln("Periodic sync is disabled")
		return
	}
	br.Log.Debugln("Periodic sync is enabled")
	for {
		time.Sleep(time.Hour)
		br.Log.Infoln("Executing periodic chat/contact info sync")
		for _, portal := range br.GetAllPortals() {
			if len(portal.MXID) > 0 {
				portal.log.Infoln("Syncing portal (periodic sync, existing portal)")
				portal.Sync(false)
			}
		}
	}
}

func (br *IMBridge) UpdateBotProfile() {
	br.Log.Debugln("Updating bot profile")
	botConfig := br.Config.AppService.Bot

	var err error
	if botConfig.Avatar == "remove" {
		err = br.Bot.SetAvatarURL(id.ContentURI{})
	} else if len(botConfig.Avatar) > 0 && !botConfig.ParsedAvatar.IsEmpty() {
		err = br.Bot.SetAvatarURL(botConfig.ParsedAvatar)
	}
	if err != nil {
		br.Log.Warnln("Failed to update bot avatar:", err)
	}

	if botConfig.Displayname == "remove" {
		err = br.Bot.SetDisplayName("")
	} else if len(botConfig.Avatar) > 0 {
		err = br.Bot.SetDisplayName(botConfig.Displayname)
	}
	if err != nil {
		br.Log.Warnln("Failed to update bot displayname:", err)
	}
}

func (br *IMBridge) ipcStop(_ json.RawMessage) interface{} {
	br.Stop()
	return nil
}

func (br *IMBridge) Stop() {
	select {
	case br.stop <- struct{}{}:
	default:
	}
}

func (br *IMBridge) internalStop() {
	br.stopping = true
	if br.Crypto != nil {
		br.Crypto.Stop()
	}
	select {
	case br.stopPinger <- struct{}{}:
	default:
	}
	br.Log.Debugln("Stopping transaction websocket")
	br.AS.StopWebsocket(appservice.ErrWebsocketManualStop)
	br.Log.Debugln("Stopping event processor")
	br.EventProcessor.Stop()
	br.Log.Debugln("Stopping iMessage connector")
	br.IM.Stop()
	br.IMHandler.Stop()
	// Short-circuit reconnect backoff so the websocket loop exits even if it's disconnected
	select {
	case br.shortCircuitReconnectBackoff <- struct{}{}:
	default:
	}
	select {
	case <-br.websocketStopped:
	case <-time.After(4 * time.Second):
		br.Log.Warnln("Timed out waiting for websocket to close")
	}
}

func (br *IMBridge) HandleFlags() bool {
	if *checkPermissions {
		checkMacPermissions()
		return true
	}
	if len(*configURL) > 0 {
		err := config.Download(*configURL, br.ConfigPath, *configOutputRedirect)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Failed to download config: %v\n", err)
			os.Exit(2)
		}
	}
	return false
}

func main() {
	br := &IMBridge{
		portalsByMXID: make(map[id.RoomID]*Portal),
		portalsByGUID: make(map[string]*Portal),
		puppets:       make(map[string]*Puppet),
		userCache:     make(map[id.UserID]*User),
		stop:          make(chan struct{}, 1),

		shortCircuitReconnectBackoff: make(chan struct{}),
		websocketStarted:             make(chan struct{}),
		websocketStopped:             make(chan struct{}),
	}
	br.Bridge = bridge.Bridge{
		Name: "mautrix-imessage",

		URL:          "https://github.com/mautrix/imessage",
		Description:  "A Matrix-iMessage puppeting bridge.",
		Version:      "0.1.0",
		ProtocolName: "iMessage",

		AdditionalShortFlags: "po",
		AdditionalLongFlags:  " [-u <url>]",

		CryptoPickleKey: "go.mau.fi/mautrix-imessage",

		ConfigUpgrader: &configupgrade.StructUpgrader{
			SimpleUpgrader: configupgrade.SimpleUpgrader(config.DoUpgrade),
			Blocks:         config.SpacedBlocks,
			Base:           ExampleConfig,
		},

		Child: br,
	}
	br.InitVersion(Tag, Commit, BuildTime)

	br.Main()
}
