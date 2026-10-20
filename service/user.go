package service

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type UserService struct {
	db          *gorm.DB
	issuer      *auth.Issuer
	issuanceErr error
	authTimeout time.Duration
}

var errLastAdminDelete = errors.New("The last admin user cannot be deleted")

// InfoById 根据用户id取用户信息
func (us *UserService) InfoById(id uint) *model.User {
	u := &model.User{}
	DB.Where("id = ?", id).First(u)
	return u
}

// InfoByUsername 根据用户名取用户信息
func (us *UserService) InfoByUsername(un string) *model.User {
	u := &model.User{}
	DB.Where("username = ?", un).First(u)
	return u
}

// InfoByEmail 根据邮箱取用户信息
func (us *UserService) InfoByEmail(email string) *model.User {
	u := &model.User{}
	DB.Where("email = ?", email).First(u)
	return u
}

// InfoByOpenid 根据openid取用户信息
func (us *UserService) InfoByOpenid(openid string) *model.User {
	u := &model.User{}
	DB.Where("openid = ?", openid).First(u)
	return u
}

// InfoByUsernamePassword 根据用户名密码取用户信息
func (us *UserService) InfoByUsernamePassword(username, password string) *model.User {
	if Config.Ldap.Enable {
		u, err := AllService.LdapService.Authenticate(username, password)
		if err == nil {
			return u
		}
		Logger.Errorf("LDAP authentication failed, %v", err)
		Logger.Warn("Fallback to local database")
	}
	u := &model.User{}
	DB.Where("username = ?", username).First(u)
	if u.Id == 0 {
		return u
	}
	ok, newHash, err := utils.VerifyPassword(u.Password, password)
	if err != nil || !ok {
		return &model.User{}
	}
	if newHash != "" {
		DB.Model(u).Update("password", newHash)
		u.Password = newHash
	}
	return u
}

// InfoByAccesstoken 根据accesstoken取用户信息
func (us *UserService) InfoByAccessToken(token string) (*model.User, *model.UserToken) {
	u := &model.User{}
	ut := &model.UserToken{}
	DB.Where("token = ?", token).First(ut)
	if ut.Id == 0 {
		return u, ut
	}
	if ut.ExpiredAt < time.Now().Unix() {
		return u, ut
	}
	DB.Where("id = ?", ut.UserId).First(u)
	return u, ut
}

// Login 登录
func (us *UserService) Login(ctx context.Context, u *model.User, llog *model.LoginLog) (*model.UserToken, error) {
	if us == nil || us.issuer == nil || us.issuanceErr != nil {
		return nil, auth.ErrUnavailable
	}
	if u == nil || llog == nil || u.Id == 0 {
		return nil, auth.ErrInvalidInput
	}
	ctx, cancel := us.operationContext(ctx)
	defer cancel()
	expected := u.Password
	p, err := us.issuer.Issue(ctx, auth.IssueRequest{UserID: u.Id, ExpectedPassword: &expected,
		DeviceID: llog.DeviceId, DeviceUUID: llog.Uuid, Client: llog.Client,
		Platform: llog.Platform, IP: llog.Ip, Method: llog.Type})
	if err != nil {
		return nil, err
	}
	admin := p.User.IsAdmin
	*u = model.User{IdModel: model.IdModel{Id: p.User.ID}, Username: p.User.Username,
		Email: p.User.Email, Password: p.User.PasswordHash, Nickname: p.User.Nickname,
		Avatar: p.User.Avatar, GroupId: p.User.GroupID, IsAdmin: &admin,
		Status: model.COMMON_STATUS_ENABLE, Remark: p.User.Remark,
		TimeModel: model.TimeModel{CreatedAt: custom_types.AutoTime(p.User.CreatedAt), UpdatedAt: custom_types.AutoTime(p.User.UpdatedAt)}}
	llog.UserId, llog.UserTokenId = p.User.ID, p.Session.ID
	return &model.UserToken{IdModel: model.IdModel{Id: p.Session.ID}, UserId: p.Session.UserID,
		Token: p.Session.Token, DeviceUuid: p.Session.DeviceUUID, DeviceId: p.Session.DeviceID,
		ExpiredAt: p.Session.ExpiresAt.Unix()}, nil
}

func (us *UserService) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := us.authTimeout
	if budget <= 0 {
		budget = 5 * time.Second
	}
	return context.WithTimeout(ctx, budget)
}

// CurUser 获取当前用户
func (us *UserService) CurUser(c *gin.Context) *model.User {
	user, _ := c.Get("curUser")
	u, ok := user.(*model.User)
	if !ok {
		return nil
	}
	return u
}

func (us *UserService) List(page, pageSize uint, where func(tx *gorm.DB)) (res *model.UserList) {
	res = &model.UserList{}
	res.Page = int64(page)
	res.PageSize = int64(pageSize)
	tx := DB.Model(&model.User{})
	if where != nil {
		where(tx)
	}
	tx.Count(&res.Total)
	tx.Scopes(Paginate(page, pageSize))
	tx.Find(&res.Users)
	return
}

func (us *UserService) ListByIds(ids []uint) (res []*model.User) {
	DB.Where("id in ?", ids).Find(&res)
	return res
}

// ListByGroupId 根据组id取用户列表
func (us *UserService) ListByGroupId(groupId, page, pageSize uint) (res *model.UserList) {
	res = us.List(page, pageSize, func(tx *gorm.DB) {
		tx.Where("group_id = ?", groupId)
	})
	return
}

// ListIdsByGroupId 根据组id取用户id列表
func (us *UserService) ListIdsByGroupId(groupId uint) (ids []uint) {
	DB.Model(&model.User{}).Where("group_id = ?", groupId).Pluck("id", &ids)
	return ids

}

// ListIdAndNameByGroupId 根据组id取用户id和用户名列表
func (us *UserService) ListIdAndNameByGroupId(groupId uint) (res []*model.User) {
	DB.Model(&model.User{}).Where("group_id = ?", groupId).Select("id, username").Find(&res)
	return res
}

// CheckUserEnable 判断用户是否禁用
func (us *UserService) CheckUserEnable(u *model.User) bool {
	return u.Status == model.COMMON_STATUS_ENABLE
}

// Create 创建
func (us *UserService) Create(u *model.User) error {
	// The initial username should be formatted, and the username should be unique
	if us.IsUsernameExists(u.Username) {
		return errors.New("UsernameExists")
	}
	u.Username = us.formatUsername(u.Username)
	var err error
	if u.Password == "" {
		u.Password = utils.DefaultInitPassword
	}
	u.Password, err = utils.EncryptPassword(u.Password)
	if err != nil {
		return err
	}
	res := DB.Create(u).Error
	return res
}

// GetUuidByToken 根据token和user取uuid
func (us *UserService) GetUuidByToken(u *model.User, token string) string {
	ut := &model.UserToken{}
	err := DB.Where("user_id = ? and token = ?", u.Id, token).First(ut).Error
	if err != nil {
		return ""
	}
	return ut.DeviceUuid
}

// Logout 退出登录 -> 删除token, 解绑uuid
func (us *UserService) Logout(u *model.User, token string) error {
	uuid := us.GetUuidByToken(u, token)
	err := DB.Where("user_id = ? and token = ?", u.Id, token).Delete(&model.UserToken{}).Error
	if err != nil {
		return err
	}
	if uuid != "" {
		AllService.PeerService.UuidUnbindUserId(uuid, u.Id)
	}
	return nil
}

// Delete retains the CLI compatibility entry point.
func (us *UserService) Delete(u *model.User) error {
	return us.DeleteContext(context.Background(), u)
}

// DeleteContext removes identity, sessions and peer ownership in one transaction.
func (us *UserService) DeleteContext(ctx context.Context, u *model.User) error {
	if us == nil || us.db == nil {
		return auth.ErrUnavailable
	}
	if u == nil || u.Id == 0 {
		return auth.ErrInvalidInput
	}
	ctx, cancel := us.operationContext(ctx)
	defer cancel()
	db := us.db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(ctx)
	err := db.Transaction(func(tx *gorm.DB) error {
		var current model.User
		if err := lifecycleRows(tx).First(&current, u.Id).Error; err != nil {
			return err
		}
		// Login can hold an identity before waiting for this user. Never wait
		// for that identity in the opposite order: fail this whole deletion.
		for _, rows := range []any{&[]model.OIDCIdentity{}, &[]model.UserThird{}} {
			query := tx.Select("id").Where("user_id = ?", current.Id).Order("id ASC")
			if tx.Dialector.Name() != "sqlite" {
				query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"})
			}
			if err := query.Find(rows).Error; err != nil {
				return referenceWriteError(tx, err)
			}
		}
		if us.IsAdmin(&current) {
			var count int64
			if err := tx.Model(&model.User{}).Where("is_admin = ?", true).Count(&count).Error; err != nil {
				return err
			}
			if count <= 1 {
				return errLastAdminDelete
			}
		}
		var candidates []model.AddressBookCollection
		if err := tx.Select("id,user_id").Where("user_id = ?", current.Id).Order("id ASC").Find(&candidates).Error; err != nil {
			return err
		}
		cids := make([]uint, 0, len(candidates))
		for _, row := range candidates {
			cids = append(cids, row.Id)
		}
		if err := collectionguard.EnsureUnreferenced(tx, cids); err != nil {
			return err
		}
		// Revoke locks the token index before the session PK. Use that same
		// access path, with NOWAIT so a busy group rolls back this whole delete.
		var sessions []model.UserToken
		if err := tx.Select("id,token").Where("user_id = ?", current.Id).Order("id ASC").Find(&sessions).Error; err != nil {
			return err
		}
		groups := make(map[string]bool, len(sessions))
		candidatesByID := make(map[uint]string, len(sessions))
		for _, candidate := range sessions {
			groups[candidate.Token] = true
			candidatesByID[candidate.Id] = candidate.Token
		}
		tokens := make([]string, 0, len(groups))
		for token := range groups {
			tokens = append(tokens, token)
		}
		sort.Strings(tokens)
		lockedSessions := map[uint]bool{}
		for _, token := range tokens {
			var rows []model.UserToken
			if err := lifecycleSessionGroup(tx, token).Select("id,user_id,token").Find(&rows).Error; err != nil {
				return referenceWriteError(tx, err)
			}
			for _, row := range rows {
				if expected, exists := candidatesByID[row.Id]; exists && row.UserId == current.Id && row.Token == expected {
					lockedSessions[row.Id] = true
				}
			}
		}
		// A revoked row may disappear. A still-owned candidate whose token
		// changed cannot be silently left behind by this deletion.
		for _, candidate := range sessions {
			if lockedSessions[candidate.Id] {
				continue
			}
			var remaining model.UserToken
			if err := tx.Select("id,user_id,token").First(&remaining, candidate.Id).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}
			if remaining.UserId == current.Id {
				return collectionguard.ErrUnavailable
			}
		}
		var peers []model.Peer
		if err := tx.Select("row_id").Where("user_id = ?", current.Id).Order("row_id ASC").Find(&peers).Error; err != nil {
			return err
		}
		peerIDs := make([]uint, 0, len(peers))
		for _, candidate := range peers {
			var peer model.Peer
			if err := lifecyclePrimaryRows(tx, "peers").Select("row_id,user_id").First(&peer, candidate.RowId).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}
			if peer.UserId == current.Id {
				peerIDs = append(peerIDs, peer.RowId)
			}
		}
		for _, cid := range cids {
			var collection model.AddressBookCollection
			if err := lifecycleRows(tx).First(&collection, cid).Error; err != nil {
				return err
			}
			if collection.UserId != current.Id {
				return collectionguard.ErrInput
			}
		}
		if err := collectionguard.EnsureUnreferenced(tx, cids); err != nil {
			return err
		}
		for _, cid := range cids {
			if err := collectionguard.DeleteContents(tx, cid); err != nil {
				return err
			}
		}
		for _, row := range []interface{}{&model.OIDCIdentity{}, &model.UserThird{},
			&model.AddressBook{}, &model.AddressBookCollectionRule{}, &model.Tag{}} {
			if err := tx.Where("user_id = ?", u.Id).Delete(row).Error; err != nil {
				return err
			}
		}
		sessionIDs := make([]uint, 0, len(lockedSessions))
		for id := range lockedSessions {
			sessionIDs = append(sessionIDs, id)
		}
		sort.Slice(sessionIDs, func(i, j int) bool { return sessionIDs[i] < sessionIDs[j] })
		for _, id := range sessionIDs {
			deleted := tx.Where("id = ?", id).Delete(&model.UserToken{})
			if deleted.Error != nil {
				return deleted.Error
			}
			if deleted.RowsAffected != 1 {
				return collectionguard.ErrUnavailable
			}
		}
		for _, id := range peerIDs {
			if err := tx.Model(&model.Peer{}).Where("row_id = ?", id).Update("user_id", 0).Error; err != nil {
				return err
			}
		}
		deleted := tx.Delete(&current)
		if deleted.Error != nil {
			return deleted.Error
		}
		if deleted.RowsAffected != 1 {
			return collectionguard.ErrUnavailable
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	return referenceWriteError(db, err)
}

func lifecycleRows(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() != "sqlite" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}

// Lock a known peer PK without first taking a user_id secondary-index lock.
// The caller passes a fixed table name, never request input.
func lifecyclePrimaryRows(tx *gorm.DB, table string) *gorm.DB {
	query := lifecycleRows(tx)
	if tx.Dialector.Name() == "mysql" {
		query = query.Clauses(clause.From{Tables: []clause.Table{{Name: table + " FORCE INDEX (PRIMARY)", Raw: true}}})
	}
	return query
}

func lifecycleSessionGroup(tx *gorm.DB, token string) *gorm.DB {
	query := tx.Where("token = ?", token).Order("id ASC")
	if tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"})
	}
	if tx.Dialector.Name() == "mysql" {
		query = query.Clauses(clause.From{Tables: []clause.Table{{Name: "user_tokens FORCE INDEX (idx_user_tokens_token)", Raw: true}}})
	}
	return query
}

// Update 更新
func (us *UserService) Update(u *model.User) error {
	currentUser := us.InfoById(u.Id)
	// 如果当前用户是管理员并且 IsAdmin 不为空，进行检查
	if us.IsAdmin(currentUser) {
		adminCount := us.getAdminUserCount()
		// 如果这是唯一的管理员，确保不能禁用或取消管理员权限
		if adminCount <= 1 && (!us.IsAdmin(u) || u.Status == model.COMMON_STATUS_DISABLED) {
			return errors.New("The last admin user cannot be disabled or demoted")
		}
	}
	return DB.Model(u).Updates(u).Error
}

// FlushToken 清空token
func (us *UserService) FlushToken(u *model.User) error {
	return DB.Where("user_id = ?", u.Id).Delete(&model.UserToken{}).Error
}

// FlushTokenByUuid 清空token
func (us *UserService) FlushTokenByUuid(uuid string) error {
	return DB.Where("device_uuid = ?", uuid).Delete(&model.UserToken{}).Error
}

// FlushTokenByUuids 清空token
func (us *UserService) FlushTokenByUuids(uuids []string) error {
	return DB.Where("device_uuid in (?)", uuids).Delete(&model.UserToken{}).Error
}

// UpdatePassword retains the CLI compatibility entry point.
func (us *UserService) UpdatePassword(u *model.User, password string) error {
	return us.UpdatePasswordContext(context.Background(), u, password)
}

// UpdatePasswordContext commits the password and revocation together. The
// expected hash prevents a stale password check from overwriting a newer change.
func (us *UserService) UpdatePasswordContext(ctx context.Context, u *model.User, password string) error {
	if us == nil || us.db == nil {
		return auth.ErrUnavailable
	}
	if u == nil || u.Id == 0 {
		return auth.ErrInvalidInput
	}
	ctx, cancel := us.operationContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	hash, err := utils.EncryptPassword(password)
	if err != nil {
		return err
	}
	err = us.db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.User{}).Where("id = ? AND password = ?", u.Id, u.Password).Update("password", hash)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return auth.ErrUnauthenticated
		}
		return tx.Where("user_id = ?", u.Id).Delete(&model.UserToken{}).Error
	})
	if err == nil {
		u.Password = hash
	}
	return err
}

// IsAdmin 是否管理员
func (us *UserService) IsAdmin(u *model.User) bool {
	return u != nil && u.IsAdmin != nil && *u.IsAdmin
}

// RouteNames
func (us *UserService) RouteNames(u *model.User) []string {
	if us.IsAdmin(u) {
		return model.AdminRouteNames
	}
	return model.UserRouteNames
}

// InfoByOauthId 根据oauth的name和openId取用户信息
func (us *UserService) InfoByOauthId(op string, openId string) *model.User {
	ut := AllService.OauthService.UserThirdInfo(op, openId)
	if ut.Id == 0 {
		return nil
	}
	u := us.InfoById(ut.UserId)
	if u.Id == 0 {
		return nil
	}
	return u
}

// RegisterByOauth is deliberately disabled. The former implementation linked
// accounts by email; external admission now requires the verified issuer/sub
// identity transaction rather than this legacy provider-name lookup.
func (us *UserService) RegisterByOauth(oauthUser *model.OauthUser, op string) (error, *model.User) {
	return errors.New("legacy OAuth registration is unsupported; use verified OIDC identity admission"), nil
}

// GenerateUsernameByOauth 生成用户名
func (us *UserService) GenerateUsernameByOauth(name string) string {
	for us.IsUsernameExists(name) {
		name += strconv.Itoa(rand.Intn(10)) // Append a random digit (0-9)
	}
	return name
}

// UserThirdsByUserId
func (us *UserService) UserThirdsByUserId(userId uint) (res []*model.UserThird) {
	DB.Where("user_id = ?", userId).Find(&res)
	return res
}

func (us *UserService) UserThirdInfo(userId uint, op string) *model.UserThird {
	ut := &model.UserThird{}
	DB.Where("user_id = ? and op = ?", userId, op).First(ut)
	return ut
}

// FindLatestUserIdFromLoginLogByUuid 根据uuid和设备id查找最后登录的用户id
func (us *UserService) FindLatestUserIdFromLoginLogByUuid(uuid string, deviceId string) uint {
	llog := &model.LoginLog{}
	DB.Select("user_id").Where("uuid = ? and device_id = ?", uuid, deviceId).Order("id desc").First(llog)
	return llog.UserId
}

// IsPasswordEmptyById 根据用户id判断密码是否为空，主要用于第三方登录的自动注册
func (us *UserService) IsPasswordEmptyById(id uint) bool {
	u := &model.User{}
	if DB.Where("id = ?", id).First(u).Error != nil {
		return false
	}
	return u.Password == ""
}

// IsPasswordEmptyByUsername 根据用户id判断密码是否为空，主要用于第三方登录的自动注册
func (us *UserService) IsPasswordEmptyByUsername(username string) bool {
	u := &model.User{}
	if DB.Where("username = ?", username).First(u).Error != nil {
		return false
	}
	return u.Password == ""
}

// IsPasswordEmptyByUser 判断密码是否为空，主要用于第三方登录的自动注册
func (us *UserService) IsPasswordEmptyByUser(u *model.User) bool {
	return us.IsPasswordEmptyById(u.Id)
}

// Register 注册, 如果用户名已存在则返回nil
func (us *UserService) Register(username string, email string, password string, status model.StatusCode) *model.User {
	u := &model.User{
		Username: username,
		Email:    email,
		Password: password,
		GroupId:  1,
		Status:   status,
	}
	err := us.Create(u)
	if err != nil {
		return nil
	}
	return u
}

func (us *UserService) TokenList(page uint, size uint, f func(tx *gorm.DB)) *model.UserTokenList {
	res := &model.UserTokenList{}
	res.Page = int64(page)
	res.PageSize = int64(size)
	tx := DB.Model(&model.UserToken{})
	if f != nil {
		f(tx)
	}
	tx.Count(&res.Total)
	tx.Scopes(Paginate(page, size))
	tx.Find(&res.UserTokens)
	return res
}

func (us *UserService) TokenInfoById(id uint) *model.UserToken {
	ut := &model.UserToken{}
	DB.Where("id = ?", id).First(ut)
	return ut
}

func (us *UserService) DeleteToken(l *model.UserToken) error {
	return DB.Delete(l).Error
}

// Helper functions, used for formatting username
func (us *UserService) formatUsername(username string) string {
	username = strings.ReplaceAll(username, " ", "")
	username = strings.ToLower(username)
	return username
}

// Helper functions, getUserCount
func (us *UserService) getUserCount() int64 {
	var count int64
	DB.Model(&model.User{}).Count(&count)
	return count
}

// helper functions, getAdminUserCount
func (us *UserService) getAdminUserCount() int64 {
	var count int64
	DB.Model(&model.User{}).Where("is_admin = ?", true).Count(&count)
	return count
}

// UserTokenExpireTimestamp 生成用户token过期时间
func (us *UserService) UserTokenExpireTimestamp() int64 {
	exp := Config.App.TokenExpire
	if exp == 0 {
		//默认七天
		exp = 168 * time.Hour
	}
	return time.Now().Add(exp).Unix()
}

func (us *UserService) RefreshAccessToken(ut *model.UserToken) {
	ut.ExpiredAt = us.UserTokenExpireTimestamp()
	DB.Model(ut).Update("expired_at", ut.ExpiredAt)
}

func (us *UserService) AutoRefreshAccessToken(ut *model.UserToken) {
	if ut.ExpiredAt-time.Now().Unix() < Config.App.TokenExpire.Milliseconds()/3000 {
		us.RefreshAccessToken(ut)
	}
}

func (us *UserService) BatchDeleteUserToken(ids []uint) error {
	return DB.Where("id in ?", ids).Delete(&model.UserToken{}).Error
}

func (us *UserService) VerifyJWT(token string) (uint, error) {
	return Jwt.ParseToken(token)
}

// IsUsernameExists 判断用户名是否存在, it will check the internal database and LDAP(if enabled)
func (us *UserService) IsUsernameExists(username string) bool {
	return us.IsUsernameExistsLocal(username) || AllService.LdapService.IsUsernameExists(username)
}

func (us *UserService) IsUsernameExistsLocal(username string) bool {
	u := &model.User{}
	DB.Where("username = ?", username).First(u)
	return u.Id != 0
}

func (us *UserService) IsEmailExistsLdap(email string) bool {
	return AllService.LdapService.IsEmailExists(email)
}
