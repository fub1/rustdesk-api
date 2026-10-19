package api

import (
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/global"
	requstform "github.com/lejianwen/rustdesk-api/v2/http/request/api"
	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/service"
	"net/http"
	"strconv"
	"strings"
)

type Ab struct {
}

// Ab
// @Tags 地址
// @Summary 地址列表
// @Description 地址列表
// @Accept  json
// @Produce  json
// @Success 200 {object} response.Response
// @Failure 500 {object} response.ErrorResponse
// @Router /ab [get]
// @Security BearerAuth
func (a *Ab) Ab(c *gin.Context) {
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	peers, tags, err := store.Personal(ctx, actor)
	if err != nil {
		clientFailure(c, err)
		return
	}
	names := make([]string, 0, len(tags))
	colors := map[string]uint{}
	for _, tag := range tags {
		names = append(names, tag.Name)
		colors[tag.Name] = tag.Color
	}
	encodedColors, err := json.Marshal(colors)
	if err != nil {
		clientFailure(c, err)
		return
	}
	data, err := json.Marshal(gin.H{"peers": peers, "tags": names, "tag_colors": string(encodedColors)})
	if err != nil {
		clientFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": string(data)})
}

// UpAb
// @Tags 地址
// @Summary 地址更新
// @Description 地址更新
// @Accept  json
// @Produce  json
// @Param body body requstform.AddressBookForm true "地址表单"
// @Success 200 {string} string "null"
// @Failure 500 {object} response.ErrorResponse
// @Router /ab [post]
// @Security BearerAuth
func (a *Ab) UpAb(c *gin.Context) {
	var outer requstform.AddressBookForm
	if !clientBody(c, &outer, false) {
		clientFailure(c, clientcompat.ErrInput)
		return
	}
	var document clientcompat.Document
	if json.Unmarshal([]byte(outer.Data), &document) != nil {
		clientFailure(c, clientcompat.ErrInput)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	if err := store.ReplacePersonal(ctx, actor, document); err != nil {
		clientFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, nil)
}

// PTags
// @Tags 地址[Personal]
// @Summary 标签
// @Description 标签
// @Accept  json
// @Produce  json
// @Param guid path string true "guid"
// @Success 200 {object} model.TagList
// @Failure 500 {object} response.ErrorResponse
// @Router /ab/tags/{guid} [post]
// @Security BearerAuth
func (a *Ab) PTags(c *gin.Context) {
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	tags, err := store.Tags(ctx, actor, c.Param("guid"))
	if err != nil {
		clientFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, tags)
}

// TagAdd
// @Tags 地址[Personal]
// @Summary 标签添加
// @Description 标签
// @Accept  json
// @Produce  json
// @Param guid path string true "guid"
// @Success 200 {string} string
// @Failure 500 {object} response.ErrorResponse
// @Router /ab/tag/add/{guid} [post]
// @Security BearerAuth
func (a *Ab) TagAdd(c *gin.Context) {
	var input clientcompat.TagInput
	if !clientBody(c, &input, true) || !input.Name.Present || input.Color == nil {
		clientFailure(c, clientcompat.ErrInput)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	if err := store.AddTag(ctx, actor, c.Param("guid"), clientcompat.Tag{Name: input.Name.Value, Color: *input.Color}); err != nil {
		clientFailure(c, err)
		return
	}
	c.String(http.StatusOK, "")
}

// TagRename
// @Tags 地址[Personal]
// @Summary 标签重命名
// @Description 标签
// @Accept  json
// @Produce  json
// @Param guid path string true "guid"
// @Success 200 {string} string
// @Failure 500 {object} response.ErrorResponse
// @Router /ab/tag/rename/{guid} [put]
// @Security BearerAuth
func (a *Ab) TagRename(c *gin.Context) {
	var input clientcompat.TagRenameInput
	if !clientBody(c, &input, true) || !input.Old.Present || !input.New.Present {
		clientFailure(c, clientcompat.ErrInput)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	if err := store.RenameTag(ctx, actor, c.Param("guid"), input.Old.Value, input.New.Value); err != nil {
		clientFailure(c, err)
		return
	}
	c.String(http.StatusOK, "")
}

// TagUpdate
// @Tags 地址[Personal]
// @Summary 标签修改颜色
// @Description 标签
// @Accept  json
// @Produce  json
// @Param guid path string true "guid"
// @Success 200 {string} string
// @Failure 500 {object} response.ErrorResponse
// @Router /ab/tag/update/{guid} [put]
// @Security BearerAuth
func (a *Ab) TagUpdate(c *gin.Context) {
	var input clientcompat.TagInput
	if !clientBody(c, &input, true) || !input.Name.Present || input.Color == nil {
		clientFailure(c, clientcompat.ErrInput)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	if err := store.ColorTag(ctx, actor, c.Param("guid"), clientcompat.Tag{Name: input.Name.Value, Color: *input.Color}); err != nil {
		clientFailure(c, err)
		return
	}
	c.String(http.StatusOK, "")
}

// TagDel
// @Tags 地址[Personal]
// @Summary 标签删除
// @Description 标签
// @Accept  json
// @Produce  json
// @Param guid path string true "guid"
// @Success 200 {string} string
// @Failure 500 {object} response.ErrorResponse
// @Router /ab/tag/{guid} [delete]
// @Security BearerAuth
func (a *Ab) TagDel(c *gin.Context) {
	var names []string
	if !clientBody(c, &names, true) {
		clientFailure(c, clientcompat.ErrInput)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	if err := store.DeleteTags(ctx, actor, c.Param("guid"), names); err != nil {
		clientFailure(c, err)
		return
	}
	c.String(http.StatusOK, "")
}

// Personal
// @Tags 地址[Personal]
// @Summary 个人地址
// @Description 个人地址
// @Accept  json
// @Produce  json
// @Param string body string false  "string valid"
// @Success 200 {object} response.Response
// @Failure 500 {object} response.Response
// @Router /ab/personal [post]
// @Security BearerAuth
func (a *Ab) Personal(c *gin.Context) {
	user := service.AllService.UserService.CurUser(c)
	/**
	guid = json['guid'] ?? '',
	       name = json['name'] ?? '',
	       owner = json['owner'] ?? '',
	       note = json['note'] ?? '',
	       rule = json['rule'] ?? 0;
	*/
	if global.Config.Rustdesk.Personal == 1 {
		guid := a.ComposeGuid(user.GroupId, user.Id, 0)
		//如果返回了guid，后面的请求会有变化
		c.JSON(http.StatusOK, gin.H{
			"guid": guid,
			"name": user.Username,
			"rule": 3,
		})
	} else {
		c.JSON(http.StatusOK, nil)
	}

}

// Settings
// @Tags 地址[Personal]
// @Summary 设置
// @Description 设置
// @Accept  json
// @Produce  json
// @Param string body string false  "string valid"
// @Success 200 {object} response.Response
// @Failure 500 {object} response.Response
// @Router /ab/settings [post]
// @Security BearerAuth
func (a *Ab) Settings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"max_peer_one_ab": 0, //最大peer数，0表示不限制
	})
}

// SharedProfiles
// @Tags 地址[Personal]
// @Summary 共享地址簿
// @Description 共享
// @Accept  json
// @Produce  json
// @Param current query int false "页码"
// @Param pageSize query int false "每页数量"
// @Success 200 {object} response.Response
// @Failure 500 {object} response.Response
// @Router /ab/shared/profiles [post]
// @Security BearerAuth
func (a *Ab) SharedProfiles(c *gin.Context) {
	page, err := requstform.ClientPage(c.Request.URL.Query())
	if err != nil {
		clientFailure(c, err)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	total, rows, err := store.Profiles(ctx, actor, page)
	if err != nil {
		clientFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "data": rows})
}

// ParseGuid
func (a *Ab) ParseGuid(guid string) (gid, uid, cid uint) {
	//用-切割 guid
	guids := strings.Split(guid, "-")
	if len(guids) < 2 {
		return 0, 0, 0
	}
	if len(guids) != 3 {
		cid = 0
	} else {
		s, err := strconv.Atoi(guids[2])
		if err != nil {
			return 0, 0, 0
		}
		cid = uint(s)
	}
	g, err := strconv.Atoi(guids[0])
	if err != nil {
		return 0, 0, 0
	}
	gid = uint(g)
	u, err := strconv.Atoi(guids[1])
	if err != nil {
		return 0, 0, 0
	}
	uid = uint(u)
	return
}

// ComposeGuid
func (a *Ab) ComposeGuid(gid, uid, cid uint) string {
	return strconv.Itoa(int(gid)) + "-" + strconv.Itoa(int(uid)) + "-" + strconv.Itoa(int(cid))
}

// CheckGuid
func (a *Ab) CheckGuid(cu *model.User, guid string) (gid, uid, cid uint, err error) {
	gid, uid, cid = a.ParseGuid(guid)
	err = nil
	if gid == 0 || uid == 0 {
		err = errors.New("ParamsError")
		return
	}
	u := &model.User{}
	if cu.Id == uid {
		u = cu
	} else {
		u = service.AllService.UserService.InfoById(uid)
	}
	if u == nil || u.Id == 0 {
		err = errors.New("ParamsError")
		return
	}
	if u.GroupId != gid {
		err = errors.New("ParamsError")
		return
	}
	if cid == 0 && cu.Id != uid {
		err = errors.New("ParamsError")
		return
	}
	if cid > 0 {
		c := service.AllService.AddressBookService.CollectionInfoById(cid)
		if c == nil || c.Id == 0 {
			err = errors.New("ParamsError")
			return
		}
		if c.UserId != uid {
			err = errors.New("ParamsError")
			return
		}
	}
	return
}

// Peers
// @Tags 地址[Personal]
// @Summary 地址列表
// @Description 地址
// @Accept  json
// @Produce  json
// @Param current query int false "页码"
// @Param pageSize query int false "每页数量"
// @Param ab query string false "guid"
// @Success 200 {object} response.Response
// @Failure 500 {object} response.Response
// @Router /ab/peers [post]
// @Security BearerAuth
func (a *Ab) Peers(c *gin.Context) {
	page, err := requstform.ClientPage(c.Request.URL.Query())
	if err != nil {
		clientFailure(c, err)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	total, rows, err := store.Peers(ctx, actor, c.Query("ab"), page)
	if err != nil {
		clientFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "data": rows, "licensed_devices": 99999})
}

// PeerAdd
// @Tags 地址[Personal]
// @Summary 添加地址
// @Description 添加地址
// @Accept  json
// @Produce  json
// @Param guid path string true "guid"
// @Success 200 {string} string
// @Failure 500 {object} response.ErrorResponse
// @Router /ab/peer/add/{guid} [post]
// @Security BearerAuth
func (a *Ab) PeerAdd(c *gin.Context) {
	var input clientcompat.PeerInput
	if !clientBody(c, &input, false) {
		clientFailure(c, clientcompat.ErrInput)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	if err := store.Add(ctx, actor, c.Param("guid"), input); err != nil {
		clientFailure(c, err)
		return
	}
	c.String(http.StatusOK, "")
}

// PeerDel
// @Tags 地址[Personal]
// @Summary 删除地址
// @Description 删除地址
// @Accept  json
// @Produce  json
// @Param guid path string true "guid"
// @Success 200 {string} string
// @Failure 500 {object} response.ErrorResponse
// @Router /ab/peer/add/{guid} [delete]
// @Security BearerAuth
func (a *Ab) PeerDel(c *gin.Context) {
	var ids []string
	if !clientBody(c, &ids, true) {
		clientFailure(c, clientcompat.ErrInput)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	if err := store.DeletePeers(ctx, actor, c.Param("guid"), ids); err != nil {
		clientFailure(c, err)
		return
	}
	c.String(http.StatusOK, "")
}

// PeerUpdate
// @Tags 地址[Personal]
// @Summary 更新地址
// @Description 更新地址
// @Accept  json
// @Produce  json
// @Param guid path string true "guid"
// @Success 200 {string} string
// @Failure 500 {object} response.ErrorResponse
// @Router /ab/peer/update/{guid} [put]
// @Security BearerAuth
func (a *Ab) PeerUpdate(c *gin.Context) {
	var patch clientcompat.Patch
	if !clientBody(c, &patch, true) {
		clientFailure(c, clientcompat.ErrInput)
		return
	}
	store, actor, ctx, cancel, ok := clientOperation(c)
	if !ok {
		return
	}
	defer cancel()
	if err := store.Patch(ctx, actor, c.Param("guid"), patch); err != nil {
		clientFailure(c, err)
		return
	}
	c.String(http.StatusOK, "")
}
