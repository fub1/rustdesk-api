package service

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/internal/userguard"
	"github.com/lejianwen/rustdesk-api/v2/internal/userlifecycle"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"github.com/lejianwen/rustdesk-api/v2/utils"
	"gorm.io/gorm"
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
	if us == nil {
		return auth.ErrUnavailable
	}
	storage := us.accountDB()
	if storage == nil {
		return auth.ErrUnavailable
	}
	if u == nil || u.Id == 0 {
		return auth.ErrInvalidInput
	}
	ctx, cancel := us.operationContext(ctx)
	defer cancel()
	db := storage.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(ctx)
	err := db.Transaction(func(tx *gorm.DB) error {
		locked, err := userguard.LockUsers(tx, u.Id)
		if err != nil {
			return err
		}
		current, ok := locked[u.Id]
		if !ok {
			return gorm.ErrRecordNotFound
		}
		if err := userguard.CheckRemoval(locked, current, nil); err != nil {
			return errLastAdminDelete
		}
		return userlifecycle.DeleteLocked(tx, &current)
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	return referenceWriteError(db, err)
}

// accountDB snapshots instance storage; the fallback retains the legacy LDAP
// adapter's zero-value UserService entry point. New instance stores never use it.
func (us *UserService) accountDB() *gorm.DB {
	if us != nil && us.db != nil {
		return us.db
	}
	return DB
}

func lifecycleRows(tx *gorm.DB) *gorm.DB {
	return userguard.Rows(tx)
}

// Update 更新
func (us *UserService) Update(u *model.User) error {
	return us.UpdateContext(context.Background(), u)
}
func (us *UserService) UpdateContext(ctx context.Context, u *model.User) error {
	if us == nil {
		return auth.ErrUnavailable
	}
	storage := us.accountDB()
	if storage == nil {
		return auth.ErrUnavailable
	}
	if u == nil || u.Id == 0 {
		return auth.ErrInvalidInput
	}
	ctx, cancel := us.operationContext(ctx)
	defer cancel()
	db := storage.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(ctx)
	err := db.Transaction(func(tx *gorm.DB) error {
		locked, err := userguard.LockUsers(tx, u.Id)
		if err != nil {
			return err
		}
		current, ok := locked[u.Id]
		if !ok {
			return gorm.ErrRecordNotFound
		}
		next, values := userguard.Patch(current, *u)
		if err := userguard.CheckRemoval(locked, current, &next); err != nil {
			return err
		}
		if len(values) > 0 {
			if err := tx.Model(&model.User{}).Where("id = ?", current.Id).Updates(values).Error; err != nil {
				return err
			}
		}
		if next.Status != model.COMMON_STATUS_ENABLE {
			return userlifecycle.RevokeLocked(tx, current.Id)
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if errors.Is(err, userguard.ErrLastEnabledAdmin) {
		return userguard.ErrLastEnabledAdmin
	}
	return referenceWriteError(db, err)
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
	if us == nil {
		return auth.ErrUnavailable
	}
	storage := us.accountDB()
	if storage == nil {
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
	db := storage.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(ctx)
	err = db.Transaction(func(tx *gorm.DB) error {
		var current model.User
		if err := lifecycleRows(tx).First(&current, u.Id).Error; err != nil {
			return err
		}
		if current.Password != u.Password {
			return auth.ErrUnauthenticated
		}
		result := tx.Model(&model.User{}).Where("id = ? AND password = ?", u.Id, u.Password).Update("password", hash)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return auth.ErrUnauthenticated
		}
		return userlifecycle.RevokeLocked(tx, u.Id)
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err == nil {
		u.Password = hash
		return nil
	}
	if errors.Is(err, auth.ErrUnauthenticated) {
		return auth.ErrUnauthenticated
	}
	return referenceWriteError(db, err)
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
