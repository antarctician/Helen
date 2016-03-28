// Copyright (C) 2015  TF2Stadium
// Use of this source code is governed by the GPLv3
// that can be found in the COPYING file.

package handler

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/TF2Stadium/Helen/config"
	"github.com/TF2Stadium/Helen/controllers/broadcaster"
	chelpers "github.com/TF2Stadium/Helen/controllers/controllerhelpers"
	"github.com/TF2Stadium/Helen/controllers/controllerhelpers/hooks"
	db "github.com/TF2Stadium/Helen/database"
	"github.com/TF2Stadium/Helen/helpers"
	"github.com/TF2Stadium/Helen/models"
	"github.com/TF2Stadium/Helen/routes/socket"
	"github.com/TF2Stadium/servemetf"
	"github.com/TF2Stadium/wsevent"
)

type Lobby struct{}

func (Lobby) Name(s string) string {
	return string((s[0])+32) + s[1:]
}

var (
	reSteamGroup = regexp.MustCompile(`steamcommunity\.com\/groups\/(.+)`)
	reTwitchChan = regexp.MustCompile(`twitch.tv\/(.+)`)
	reServer     = regexp.MustCompile(`\w+\:\d+`)
	playermap    = map[string]models.LobbyType{
		"debug":      models.LobbyTypeDebug,
		"6s":         models.LobbyTypeSixes,
		"highlander": models.LobbyTypeHighlander,
		"ultiduo":    models.LobbyTypeUltiduo,
		"bball":      models.LobbyTypeBball,
		"4v4":        models.LobbyTypeFours,
	}
)

type Restriction struct {
	Red bool `json:"red,omitempty"`
	Blu bool `json:"blu,omitempty"`
}
type Requirement struct {
	Hours      int         `json:"hours"`
	Lobbies    int         `json:"lobbies"`
	Restricted Restriction `json:"restricted"`
}

type servemeServer struct {
	StartsAt string `json:"startsAt"`
	EndsAt   string `json:"endsAt"`
	Server   servemetf.Server
}

func newRequirement(team, class string, requirement Requirement, lobby *models.Lobby) error {
	slot, err := models.LobbyGetPlayerSlot(lobby.Type, team, class)
	if err != nil {
		return err
	}
	slotReq := &models.Requirement{
		LobbyID: lobby.ID,
		Slot:    slot,
		Hours:   int(requirement.Hours),
		Lobbies: int(requirement.Lobbies),
	}
	slotReq.Save()

	return nil
}

func isTwitchChannelValid(name string) bool {
	resp, err := helpers.HTTPClient.Get("https://api.twitch.tv/kraken/channels/" + name)
	return err == nil && resp.StatusCode != 404
}

func (Lobby) LobbyCreate(so *wsevent.Client, args struct {
	Map         *string        `json:"map"`
	Type        *string        `json:"type" valid:"debug,6s,highlander,4v4,ultiduo,bball"`
	League      *string        `json:"league" valid:"ugc,etf2l,esea,asiafortress,ozfortress,bballtf"`
	ServerType  *string        `json:"serverType" valid:"server,storedServer,serveme"`
	Serveme     *servemeServer `json:"serveme" empty:"-"`
	Server      *string        `json:"server" empty:"-"`
	RconPwd     *string        `json:"rconpwd" empty:"-"`
	WhitelistID *string        `json:"whitelistID"`
	Mumble      *bool          `json:"mumbleRequired"`

	Password            *string `json:"password" empty:"-"`
	SteamGroupWhitelist *string `json:"steamGroupWhitelist" empty:"-"`
	// restrict lobby slots to twitch subs for a particular channel
	// not a pointer, since it is set to false when the argument json
	// string doesn't have the field
	TwitchWhitelistSubscribers bool `json:"twitchWhitelistSubs"`
	TwitchWhitelistFollowers   bool `json:"twitchWhitelistFollows"`

	Requirements *struct {
		Classes map[string]Requirement `json:"classes,omitempty"`
		General Requirement            `json:"general,omitempty"`
	} `json:"requirements" empty:"-"`
}) interface{} {

	player := chelpers.GetPlayer(so.Token)
	if banned, until := player.IsBannedWithTime(models.PlayerBanCreate); banned {
		ban, _ := player.GetActiveBan(models.PlayerBanCreate)
		return fmt.Errorf("You've been banned from creating lobbies till %s (%s)", until.Format(time.RFC822), ban.Reason)
	}

	if player.HasCreatedLobby() {
		if player.Role != helpers.RoleAdmin && player.Role != helpers.RoleMod {
			return errors.New("You have already created a lobby.")
		}
	}

	var steamGroup string
	var context *servemetf.Context
	var reservation servemetf.Reservation

	if *args.SteamGroupWhitelist != "" {
		if reSteamGroup.MatchString(*args.SteamGroupWhitelist) {
			steamGroup = reSteamGroup.FindStringSubmatch(*args.SteamGroupWhitelist)[1]
		} else {
			return errors.New("Invalid Steam group URL")
		}
	}

	if *args.ServerType == "serveme" {
		if args.Serveme == nil {
			return errors.New("No serveme info given.")
		}
		var err error
		var start, end time.Time

		if start, err = time.Parse(servemetf.TimeFormat, (*args.Serveme).StartsAt); err != nil {
			return err
		}
		if end, err = time.Parse(servemetf.TimeFormat, (*args.Serveme).EndsAt); err != nil {
			return err
		}

		randBytes := make([]byte, 6)
		rand.Read(randBytes)
		*args.RconPwd = base64.URLEncoding.EncodeToString(randBytes)

		reservation = servemetf.Reservation{
			StartsAt:    start.Format(servemetf.TimeFormat),
			EndsAt:      end.Format(servemetf.TimeFormat),
			ServerID:    (*args.Serveme).Server.ID,
			WhitelistID: 1,
			RCON:        *args.RconPwd,
			Password:    "foobar",
		}

		context = helpers.GetServemeContextIP(chelpers.GetIPAddr(so.Request))
		resp, err := context.Create(reservation, so.Token.Claims["steam_id"].(string))
		if err != nil || resp.Reservation.Errors != nil {
			if err != nil {
				logrus.Error(err)
			} else {
				logrus.Error(resp.Reservation.Errors)
			}

			return errors.New("Couldn't get serveme reservation")
		}

		*args.Server = resp.Reservation.Server.IPAndPort
		reservation = resp.Reservation
	} else if *args.ServerType == "storedServer" {
		if *args.Server == "" {
			return errors.New("No server ID given")
		}

		id, err := strconv.ParseUint(*args.Server, 10, 64)
		if err != nil {
			return err
		}
		server, err := models.GetStoredServer(uint(id))
		if err != nil {
			return err
		}
		*args.Server = server.Address
		*args.RconPwd = server.RCONPassword
	} else { // *args.ServerType == "server"
		if args.RconPwd == nil || *args.RconPwd == "" {
			return errors.New("RCON Password cannot be empty")
		}
		if args.Server == nil || *args.Server == "" {
			return errors.New("Server Address cannot be empty")
		}
	}

	var count int

	lobbyType := playermap[*args.Type]
	db.DB.Table("server_records").Where("host = ?", *args.Server).Count(&count)
	if count != 0 {
		return errors.New("A lobby is already using this server.")
	}

	randBytes := make([]byte, 6)
	rand.Read(randBytes)
	serverPwd := base64.URLEncoding.EncodeToString(randBytes)

	//TODO what if playermap[lobbytype] is nil?
	info := models.ServerRecord{
		Host:           *args.Server,
		RconPassword:   *args.RconPwd,
		ServerPassword: serverPwd,
	}

	lob := models.NewLobby(*args.Map, lobbyType, *args.League, info, *args.WhitelistID, *args.Mumble, steamGroup, *args.Password)

	if args.TwitchWhitelistSubscribers || args.TwitchWhitelistFollowers {
		if player.TwitchName == "" {
			return errors.New("Please connect your twitch account first.")
		}

		lob.TwitchChannel = player.TwitchName
		if args.TwitchWhitelistFollowers {
			lob.TwitchRestriction = models.TwitchFollowers
		} else {
			lob.TwitchRestriction = models.TwitchSubscribers
		}
	}

	lob.CreatedBySteamID = player.SteamID
	lob.RegionCode, lob.RegionName = helpers.GetRegion(*args.Server)
	if (lob.RegionCode == "" || lob.RegionName == "") && config.Constants.GeoIP {
		return errors.New("Couldn't find the region for this server.")
	}

	if models.MapRegionFormatExists(lob.MapName, lob.RegionCode, lob.Type) {
		if reservation.ID != 0 {
			err := context.Delete(reservation.ID, player.SteamID)
			for err != nil {
				err = context.Delete(reservation.ID, player.SteamID)
			}
		}

		return errors.New("Your region already has a lobby with this map and format.")
	}

	if *args.ServerType == "serveme" {
		lob.ServemeID = reservation.ID
	}

	lob.Save()
	lob.CreateLock()

	if *args.ServerType == "serveme" {
		now := time.Now()

		for {
			status, err := context.Status(reservation.ID, player.SteamID)
			if err != nil {
				logrus.Error(err)
			}
			if status == "ready" {
				break
			}

			time.Sleep(10 * time.Second)
			if time.Since(now) >= 3*time.Minute {
				lob.Delete()
				return errors.New("Couldn't get Serveme reservation, try another server.")
			}
		}

		lob.ServemeCheck(context)
	}

	err := lob.SetupServer()
	if err != nil { //lobby setup failed, delete lobby and corresponding server record
		lob.Delete()
		return err
	}

	lob.SetState(models.LobbyStateWaiting)

	if args.Requirements != nil {
		for class, requirement := range (*args.Requirements).Classes {
			if requirement.Restricted.Blu {
				newRequirement("blu", class, requirement, lob)
			}
			if requirement.Restricted.Red {
				newRequirement("red", class, requirement, lob)
			}
		}
		if args.Requirements.General.Hours != 0 || args.Requirements.General.Lobbies != 0 {
			for i := 0; i < 2*models.NumberOfClassesMap[lob.Type]; i++ {
				req := &models.Requirement{
					LobbyID: lob.ID,
					Hours:   args.Requirements.General.Hours,
					Lobbies: args.Requirements.General.Lobbies,
					Slot:    i,
				}
				req.Save()
			}

		}
	}
	return newResponse(
		struct {
			ID uint `json:"id"`
		}{lob.ID})
}

func (Lobby) LobbyServerReset(so *wsevent.Client, args struct {
	ID *uint `json:"id"`
}) interface{} {

	player := chelpers.GetPlayer(so.Token)
	lobby, tperr := models.GetLobbyByID(*args.ID)

	if player.SteamID != lobby.CreatedBySteamID && (player.Role != helpers.RoleAdmin && player.Role != helpers.RoleMod) {
		return errors.New("You are not authorized to reset server.")
	}

	if tperr != nil {
		return tperr
	}

	if lobby.State == models.LobbyStateEnded {
		return errors.New("Lobby has ended")
	}

	if err := models.ReExecConfig(lobby.ID, false); err != nil {
		return err
	}

	return emptySuccess
}

var validAddress = regexp.MustCompile(`.+\:\d+`)

func (Lobby) ServerVerify(so *wsevent.Client, args struct {
	Server  *string `json:"server"`
	Rconpwd *string `json:"rconpwd"`
}) interface{} {

	if !validAddress.MatchString(*args.Server) {
		return errors.New("Invalid Server Address")
	}

	var count int
	db.DB.Table("server_records").Where("host = ?", *args.Server).Count(&count)
	if count != 0 {
		return errors.New("A lobby is already using this server.")
	}

	info := &models.ServerRecord{
		Host:         *args.Server,
		RconPassword: *args.Rconpwd,
	}
	db.DB.Save(info)
	defer db.DB.Delete(info)

	err := models.VerifyInfo(*info)
	if err != nil {
		return err
	}

	return emptySuccess
}

func (Lobby) LobbyClose(so *wsevent.Client, args struct {
	Id *uint `json:"id"`
}) interface{} {

	player := chelpers.GetPlayer(so.Token)
	lob, tperr := models.GetLobbyByIDServer(uint(*args.Id))
	if tperr != nil {
		return tperr
	}

	if player.SteamID != lob.CreatedBySteamID && player.Role != helpers.RoleAdmin {
		return errors.New("Player not authorized to close lobby.")

	}

	if lob.State == models.LobbyStateEnded {
		return errors.New("Lobby already closed.")
	}

	lob.Close(true, false)

	notify := fmt.Sprintf("Lobby closed by %s", player.Alias())
	models.SendNotification(notify, int(lob.ID))

	return emptySuccess
}

func (Lobby) LobbyJoin(so *wsevent.Client, args struct {
	Id       *uint   `json:"id"`
	Class    *string `json:"class"`
	Team     *string `json:"team" valid:"red,blu"`
	Password *string `json:"password" empty:"-"`
}) interface{} {

	player := chelpers.GetPlayer(so.Token)
	if banned, until := player.IsBannedWithTime(models.PlayerBanJoin); banned {
		ban, _ := player.GetActiveBan(models.PlayerBanJoin)
		return fmt.Errorf("You have been banned from joining lobbies till %s (%s)", until.Format(time.RFC822), ban.Reason)
	}

	//logrus.Debug("id %d class %s team %s", *args.Id, *args.Class, *args.Team)
	lob, tperr := models.GetLobbyByID(*args.Id)

	if tperr != nil {
		return tperr
	}

	if lob.Mumble {
		if banned, until := player.IsBannedWithTime(models.PlayerBanJoinMumble); banned {
			ban, _ := player.GetActiveBan(models.PlayerBanJoinMumble)
			return fmt.Errorf("You have been banned from joining Mumble lobbies till %s (%s)", until.Format(time.RFC822), ban.Reason)
		}
	}

	if lob.State == models.LobbyStateEnded {
		return errors.New("Cannot join a closed lobby.")

	}
	if lob.State == models.LobbyStateInitializing {
		return errors.New("Lobby is being setup right now.")
	}

	//Check if player is in the same lobby
	var sameLobby bool
	if id, err := player.GetLobbyID(false); err == nil && id == *args.Id {
		sameLobby = true
	}

	slot, tperr := models.LobbyGetPlayerSlot(lob.Type, *args.Team, *args.Class)
	if tperr != nil {
		return tperr
	}

	if prevId, _ := player.GetLobbyID(false); prevId != 0 && !sameLobby {
		lobby, _ := models.GetLobbyByID(prevId)
		hooks.AfterLobbyLeave(lobby, player)
	}

	tperr = lob.AddPlayer(player, slot, *args.Password)

	if tperr != nil {
		return tperr
	}

	if !sameLobby {
		hooks.AfterLobbyJoin(so, lob, player)
	}

	//check if lobby isn't already in progress (which happens when the player is subbing)
	lob.Lock()
	if lob.IsFull() && lob.State != models.LobbyStateInProgress && lob.State != models.LobbyStateReadyingUp {
		lob.State = models.LobbyStateReadyingUp
		lob.ReadyUpTimestamp = time.Now().Unix() + 30
		lob.Save()

		helpers.GlobalWait.Add(1)
		time.AfterFunc(time.Second*30, func() {
			state := lob.CurrentState()
			//if all player's haven't readied up,
			//remove unreadied players and unready the
			//rest.
			//don't do this when:
			//  lobby.State == Waiting (someone already unreadied up, so all players have been unreadied)
			// lobby.State == InProgress (all players have readied up, so the lobby has started)
			// lobby.State == Ended (the lobby has been closed)
			if state != models.LobbyStateWaiting && state != models.LobbyStateInProgress && state != models.LobbyStateEnded {
				lob.SetState(models.LobbyStateWaiting)
				removeUnreadyPlayers(lob)
				lob.UnreadyAllPlayers()
				//get updated lobby object
				lob, _ = models.GetLobbyByID(lob.ID)
				models.BroadcastLobby(lob)
			}
			helpers.GlobalWait.Done()
		})

		room := fmt.Sprintf("%s_private",
			hooks.GetLobbyRoom(lob.ID))
		broadcaster.SendMessageToRoom(room, "lobbyReadyUp",
			struct {
				Timeout int `json:"timeout"`
			}{30})
		models.BroadcastLobbyList()
	}
	lob.Unlock()

	if lob.State == models.LobbyStateInProgress { //this happens when the player is a substitute
		db.DB.Preload("ServerInfo").First(lob, lob.ID)
		so.EmitJSON(helpers.NewRequest("lobbyStart", models.DecorateLobbyConnect(lob, player, slot)))
	}

	return emptySuccess
}

//get list of unready players, remove them from lobby (and add them as spectators)
//plus, call the after lobby leave hook for each player removed
func removeUnreadyPlayers(lobby *models.Lobby) {
	players := lobby.GetUnreadyPlayers()
	lobby.RemoveUnreadyPlayers(true)

	for _, player := range players {
		hooks.AfterLobbyLeave(lobby, player)
	}
}

func (Lobby) LobbySpectatorJoin(so *wsevent.Client, args struct {
	Id *uint `json:"id"`
}) interface{} {

	var lob *models.Lobby
	lob, tperr := models.GetLobbyByID(*args.Id)

	if tperr != nil {
		return tperr
	}

	player := chelpers.GetPlayer(so.Token)
	var specSameLobby bool

	arr, tperr := player.GetSpectatingIds()
	if len(arr) != 0 {
		for _, id := range arr {
			if id == *args.Id {
				specSameLobby = true
				continue
			}
			//a socket should only spectate one lobby, remove socket from
			//any other lobby room
			//multiple sockets from one player can spectatte multiple lobbies
			socket.AuthServer.Leave(so, fmt.Sprintf("%d_public", id))
		}
	}

	// If the player is already in the lobby (either joined a slot or is spectating), don't add them.
	// Just Broadcast the lobby to them, so the frontend displays it.
	if id, _ := player.GetLobbyID(false); id != *args.Id && !specSameLobby {
		tperr = lob.AddSpectator(player)

		if tperr != nil {
			return tperr
		}
	}

	hooks.AfterLobbySpec(socket.AuthServer, so, player, lob)
	models.BroadcastLobbyToUser(lob, player.SteamID)
	return emptySuccess
}

func removePlayerFromLobby(lobbyId uint, steamId string) (*models.Lobby, *models.Player, error) {
	player, tperr := models.GetPlayerBySteamID(steamId)
	if tperr != nil {
		return nil, nil, tperr
	}

	lob, tperr := models.GetLobbyByID(lobbyId)
	if tperr != nil {
		return nil, nil, tperr
	}

	switch lob.State {
	case models.LobbyStateInProgress:
		return lob, player, errors.New("Lobby is in progress.")
	case models.LobbyStateEnded:
		return lob, player, errors.New("Lobby has closed.")
	}

	_, err := lob.GetPlayerSlot(player)
	if err != nil {
		return lob, player, errors.New("Player not playing")
	}

	if err := lob.RemovePlayer(player); err != nil {
		return lob, player, err
	}

	return lob, player, lob.AddSpectator(player)
}

func playerCanKick(lobbyId uint, steamId string) (bool, error) {
	lob, tperr := models.GetLobbyByID(lobbyId)
	if tperr != nil {
		return false, tperr
	}

	player, tperr2 := models.GetPlayerBySteamID(steamId)
	if tperr2 != nil {
		return false, tperr2
	}
	if steamId != lob.CreatedBySteamID && player.Role != helpers.RoleAdmin {
		return false, errors.New("Not authorized to kick players")
	}
	return true, nil
}

func (Lobby) LobbyKick(so *wsevent.Client, args struct {
	Id      *uint   `json:"id"`
	Steamid *string `json:"steamid"`
}) interface{} {

	steamId := *args.Steamid
	selfSteamId := so.Token.Claims["steam_id"].(string)

	if steamId == selfSteamId {
		return errors.New("Player can't kick himself.")
	}
	if ok, tperr := playerCanKick(*args.Id, selfSteamId); !ok {
		return tperr
	}

	lob, player, tperr := removePlayerFromLobby(*args.Id, steamId)
	if tperr != nil {
		return tperr
	}

	hooks.AfterLobbyLeave(lob, player)

	// broadcaster.SendMessage(steamId, "sendNotification",
	// 	fmt.Sprintf(`{"notification": "You have been removed from Lobby #%d"}`, *args.Id))

	return emptySuccess
}

func (Lobby) LobbyBan(so *wsevent.Client, args struct {
	Id      *uint   `json:"id"`
	Steamid *string `json:"steamid"`
}) interface{} {

	steamId := *args.Steamid
	selfSteamId := so.Token.Claims["steam_id"].(string)

	if steamId == selfSteamId {
		return errors.New("Player can't kick himself.")
	}
	if ok, tperr := playerCanKick(*args.Id, selfSteamId); !ok {
		return tperr
	}

	lob, player, tperr := removePlayerFromLobby(*args.Id, steamId)
	if tperr != nil {
		return tperr
	}

	lob.BanPlayer(player)

	hooks.AfterLobbyLeave(lob, player)

	// broadcaster.SendMessage(steamId, "sendNotification",
	// 	fmt.Sprintf(`{"notification": "You have been removed from Lobby #%d"}`, *args.Id))

	return emptySuccess
}

func (Lobby) LobbyLeave(so *wsevent.Client, args struct {
	Id *uint `json:"id"`
}) interface{} {

	steamId := so.Token.Claims["steam_id"].(string)

	lob, player, tperr := removePlayerFromLobby(*args.Id, steamId)
	if tperr != nil {
		return tperr
	}

	hooks.AfterLobbyLeave(lob, player)

	return emptySuccess
}

func (Lobby) LobbySpectatorLeave(so *wsevent.Client, args struct {
	Id *uint `json:"id"`
}) interface{} {

	player := chelpers.GetPlayer(so.Token)
	lob, tperr := models.GetLobbyByID(*args.Id)
	if tperr != nil {
		return tperr
	}

	if !player.IsSpectatingID(lob.ID) {
		if id, _ := player.GetLobbyID(false); id == *args.Id {
			hooks.AfterLobbySpecLeave(so, lob)
			return emptySuccess
		}
	}

	lob.RemoveSpectator(player, true)
	hooks.AfterLobbySpecLeave(so, lob)

	return emptySuccess
}

func (Lobby) RequestLobbyListData(so *wsevent.Client, _ struct{}) interface{} {
	so.EmitJSON(helpers.NewRequest("lobbyListData", models.DecorateLobbyListData(models.GetWaitingLobbies())))

	return emptySuccess
}

func (Lobby) LobbyChangeOwner(so *wsevent.Client, args struct {
	ID      *uint   `json:"id"`
	SteamID *string `json:"steamid"`
}) interface{} {
	lobby, err := models.GetLobbyByID(*args.ID)
	if err != nil {
		return err
	}

	player := chelpers.GetPlayer(so.Token)
	if lobby.CreatedBySteamID != player.SteamID {
		return errors.New("You aren't authorized to change lobby owner.")
	}

	player2, err := models.GetPlayerBySteamID(*args.SteamID)
	if err != nil {
		return err
	}

	lobby.CreatedBySteamID = player2.SteamID
	lobby.Save()
	models.BroadcastLobby(lobby)
	models.BroadcastLobbyList()
	models.NewBotMessage(fmt.Sprintf("Lobby leader changed to %s", player2.Alias()), int(*args.ID)).Send()

	return emptySuccess
}

func (Lobby) LobbySetRequirement(so *wsevent.Client, args struct {
	ID *uint `json:"id"` // lobby ID

	Slot  *int         `json:"slot"` // -1 if to set for all slots
	Type  *string      `json:"type"`
	Value *json.Number `json:"value"`
}) interface{} {

	lobby, tperr := models.GetLobbyByID(*args.ID)
	if tperr != nil {
		return tperr
	}

	player := chelpers.GetPlayer(so.Token)
	if lobby.CreatedBySteamID != player.SteamID {
		return errors.New("Only lobby owners can change requirements.")
	}

	if !(*args.Slot >= 0 && *args.Slot < 2*models.NumberOfClassesMap[lobby.Type]) {
		return errors.New("Invalid slot.")
	}

	req, err := lobby.GetSlotRequirement(*args.Slot)
	if err != nil { //requirement doesn't exist. create one
		req.Slot = *args.Slot
		req.LobbyID = *args.ID
	}

	var n int64
	var f float64

	switch *args.Type {
	case "hours":
		n, err = (*args.Value).Int64()
		req.Hours = int(n)
	case "lobbies":
		n, err = (*args.Value).Int64()
		req.Lobbies = int(n)
	case "reliability":
		f, err = (*args.Value).Float64()
		req.Reliability = f
	default:
		return errors.New("Invalid requirement type.")
	}

	if err != nil {
		return errors.New("Invalid requirement.")
	}

	req.Save()
	models.BroadcastLobby(lobby)
	models.BroadcastLobbyList()

	return emptySuccess
}

func (Lobby) LobbyRemoveTwitchRestriction(so *wsevent.Client, args struct {
	ID uint `json:"id"`
}) interface{} {
	player := chelpers.GetPlayer(so.Token)

	lobby, err := models.GetLobbyByID(args.ID)
	if err != nil {
		return err
	}

	if player.SteamID != lobby.CreatedBySteamID && (player.Role != helpers.RoleAdmin && player.Role != helpers.RoleMod) {
		return errors.New("You aren't authorized to do this.")
	}

	lobby.TwitchChannel = ""
	lobby.Save()

	models.BroadcastLobby(lobby)
	models.BroadcastLobbyList()

	return emptySuccess
}

func (Lobby) LobbyRemoveSteamRestriction(so *wsevent.Client, args struct {
	ID uint `json:"id"`
}) interface{} {
	player := chelpers.GetPlayer(so.Token)

	lobby, err := models.GetLobbyByID(args.ID)
	if err != nil {
		return err
	}

	if player.SteamID != lobby.CreatedBySteamID && (player.Role != helpers.RoleAdmin && player.Role != helpers.RoleMod) {
		return errors.New("You aren't authorized to do this.")
	}

	lobby.PlayerWhitelist = ""
	lobby.Save()

	models.BroadcastLobby(lobby)
	models.BroadcastLobbyList()

	return emptySuccess
}
