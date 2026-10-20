package router

import (
	"github.com/gin-gonic/gin"
	_ "github.com/lejianwen/rustdesk-api/v2/docs/admin"
	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/http/controller/admin"
	"github.com/lejianwen/rustdesk-api/v2/http/controller/admin/my"
	"github.com/lejianwen/rustdesk-api/v2/http/middleware"
	"github.com/lejianwen/rustdesk-api/v2/internal/httpapi"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

func Init(g *gin.Engine, sessions *httpapi.Handler) {

	//swagger
	//g.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	if global.Config.App.ShowSwagger == 1 {
		g.GET("/admin/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler, ginSwagger.InstanceName("admin")))
	}

	adg := g.Group("/api/admin")
	LoginBind(adg, sessions)
	adg.POST("/user/register", (&admin.User{}).Register)

	ConfigBind(adg, sessions)

	// 共享地址簿名称列表，无需认证
	adg.GET("/my/address_book_collection/list_shared", (&my.AddressBookCollection{}).ListShared)

	adg.Use(sessions.AdminAuth())
	adg.POST("/logout", sessions.AdminLogout)
	// Definition config may contain sensitive values. These explicit instance
	// handlers must be registered before the legacy request-body operation log.
	StrategyDefinitionsBind(adg, sessions)
	adg.Use(middleware.OperationLog())
	//FileBind(adg)
	UserBind(adg, sessions)
	GroupBind(adg, sessions)
	TagBind(adg, sessions)
	AddressBookBind(adg, sessions)
	PeerBind(adg, sessions)
	OauthBind(adg, sessions)
	LoginLogBind(adg, sessions)
	AuditBind(adg, sessions)
	AddressBookCollectionBind(adg, sessions)
	AddressBookCollectionRuleBind(adg, sessions)
	UserTokenBind(adg, sessions)

	//deprecated by ConfigBind
	//rs := &admin.Rustdesk{}
	//adg.GET("/server-config", rs.ServerConfig)
	//adg.GET("/app-config", rs.AppConfig)
	//deprecated end

	ShareRecordBind(adg, sessions)
	MyBind(adg)

	RustdeskCmdBind(adg, sessions)
	DeviceGroupBind(adg, sessions)
	PeerStrategyBind(adg, sessions)
	OperationLogBind(adg, sessions)
	//访问静态文件
	//g.StaticFS("/upload", http.Dir(global.Config.Gin.ResourcesPath+"/upload"))
}

func StrategyDefinitionsBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	definitions := rg.Group("/strategy-definitions").Use(sessions.RequireAdmin())
	definitions.GET("", sessions.StrategyDefinitionsList)
	definitions.POST("", sessions.StrategyDefinitionsCreate)
	definitions.GET("/:id", sessions.StrategyDefinitionsGet)
	definitions.PUT("/:id", sessions.StrategyDefinitionsUpdate)
	definitions.DELETE("/:id", sessions.StrategyDefinitionsDelete)
}

func RustdeskCmdBind(adg *gin.RouterGroup, sessions *httpapi.Handler) {
	cont := &admin.Rustdesk{}
	rg := adg.Group("/rustdesk").Use(sessions.RequireAdmin())
	rg.POST("/sendCmd", cont.SendCmd)
	rg.GET("/cmdList", cont.CmdList)
	rg.POST("/cmdDelete", cont.CmdDelete)
	rg.POST("/cmdCreate", cont.CmdCreate)
}
func LoginBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	cont := &admin.Login{}
	rg.POST("/login", cont.Login)
	rg.GET("/captcha", cont.Captcha)
	rg.GET("/login-options", cont.LoginOptions)
	rg.POST("/oidc/auth", sessions.AdminOIDCBegin)
	rg.GET("/oidc/auth-query", sessions.AdminOIDCPoll)
}

func UserBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/user")
	{
		cont := &admin.User{}
		aR.GET("/current", sessions.AdminCurrent)
		aR.POST("/changeCurPwd", cont.ChangeCurPwd)
		aR.POST("/myOauth", sessions.OIDCMyOauth)
		//aR.GET("/myPeer", cont.MyPeer)
		aR.POST("/groupUsers", cont.GroupUsers)
	}
	aRP := rg.Group("/user").Use(sessions.RequireAdmin())
	{
		cont := &admin.User{}
		aRP.GET("/list", cont.List)
		aRP.GET("/detail/:id", cont.Detail)
		aRP.POST("/create", cont.Create)
		aRP.POST("/update", cont.Update)
		aRP.POST("/delete", cont.Delete)
		aRP.POST("/changePwd", cont.UpdatePassword)
	}
}

func GroupBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/group").Use(sessions.RequireAdmin())
	{
		cont := &admin.Group{}
		aR.GET("/list", cont.List)
		aR.GET("/detail/:id", cont.Detail)
		aR.POST("/create", cont.Create)
		aR.POST("/update", cont.Update)
		aR.POST("/delete", cont.Delete)
	}
}

func DeviceGroupBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/device_group").Use(sessions.RequireAdmin())
	{
		cont := &admin.DeviceGroup{}
		aR.GET("/list", cont.List)
		aR.GET("/detail/:id", cont.Detail)
		aR.POST("/create", cont.Create)
		aR.POST("/update", cont.Update)
		aR.POST("/delete", cont.Delete)
	}
}

func TagBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/tag").Use(sessions.RequireAdmin())
	{
		cont := &admin.Tag{}
		aR.GET("/list", cont.List)
		aR.GET("/detail/:id", cont.Detail)
		aR.POST("/create", cont.Create)
		aR.POST("/update", cont.Update)
		aR.POST("/delete", cont.Delete)
	}
}

func AddressBookBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/address_book")
	{
		cont := &admin.AddressBook{}
		aR.POST("/shareByWebClient", cont.ShareByWebClient)

		arp := aR.Use(sessions.RequireAdmin())
		arp.GET("/list", cont.List)
		//arp.GET("/detail/:id", cont.Detail)
		arp.POST("/create", cont.Create)
		arp.POST("/update", cont.Update)
		arp.POST("/delete", cont.Delete)
		arp.POST("/batchCreate", cont.BatchCreate)
		arp.POST("/batchCreateFromPeers", cont.BatchCreateFromPeers)

	}
}
func PeerBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/peer")
	aR.POST("/simpleData", (&admin.Peer{}).SimpleData)
	aR.Use(sessions.RequireAdmin())
	{
		cont := &admin.Peer{}
		aR.GET("/list", cont.List)
		aR.GET("/detail/:id", cont.Detail)
		aR.POST("/create", cont.Create)
		aR.POST("/update", cont.Update)
		aR.POST("/delete", cont.Delete)
		aR.POST("/batchDelete", cont.BatchDelete)
	}
}

func OauthBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/oauth")
	{
		aR.POST("/confirm", sessions.OIDCConfirm)
		aR.POST("/bind", sessions.OIDCBind)
		aR.POST("/bindConfirm", sessions.OIDCBindConfirm)
		aR.POST("/unbind", sessions.OIDCUnbind)
		aR.GET("/info", sessions.OIDCInfo)
	}
	arp := aR.Use(sessions.RequireAdmin())
	{
		cont := &admin.Oauth{}
		arp.GET("/list", cont.List)
		arp.GET("/detail/:id", cont.Detail)
		arp.POST("/create", cont.Create)
		arp.POST("/update", cont.Update)
		arp.POST("/delete", cont.Delete)

	}

}
func LoginLogBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	cont := &admin.LoginLog{}
	aR := rg.Group("/login_log").Use(sessions.RequireAdmin())
	aR.GET("/list", cont.List)
	aR.POST("/delete", cont.Delete)
	aR.POST("/batchDelete", cont.BatchDelete)
}
func AuditBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	cont := &admin.Audit{}
	aR := rg.Group("/audit_conn").Use(sessions.RequireAdmin())
	aR.GET("/list", cont.ConnList)
	aR.POST("/delete", cont.ConnDelete)
	aR.POST("/batchDelete", cont.BatchConnDelete)
	afR := rg.Group("/audit_file").Use(sessions.RequireAdmin())
	afR.GET("/list", cont.FileList)
	afR.POST("/delete", cont.FileDelete)
	afR.POST("/batchDelete", cont.BatchFileDelete)
}
func AddressBookCollectionBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/address_book_collection").Use(sessions.RequireAdmin())
	{
		cont := &admin.AddressBookCollection{}
		aR.GET("/list", cont.List)
		aR.GET("/detail/:id", cont.Detail)
		aR.POST("/create", cont.Create)
		aR.POST("/update", cont.Update)
		aR.POST("/delete", cont.Delete)
	}

}
func AddressBookCollectionRuleBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/address_book_collection_rule").Use(sessions.RequireAdmin())
	{
		cont := &admin.AddressBookCollectionRule{}
		aR.GET("/list", cont.List)
		aR.GET("/detail/:id", cont.Detail)
		aR.POST("/create", cont.Create)
		aR.POST("/update", cont.Update)
		aR.POST("/delete", cont.Delete)
	}
}
func UserTokenBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/user_token").Use(sessions.RequireAdmin())
	cont := &admin.UserToken{}
	aR.GET("/list", cont.List)
	aR.POST("/delete", sessions.DeleteSession)
	aR.POST("/batchDelete", sessions.DeleteSessions)
}
func ConfigBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/config")
	rs := &admin.Config{}

	aR.GET("/admin", sessions.OptionalAdminAuth(), rs.AdminConfig)

	aR.Use(sessions.AdminAuth())
	aR.GET("/server", rs.ServerConfig)
	aR.GET("/app", rs.AppConfig)

}

/*
func FileBind(rg *gin.RouterGroup) {
	aR := rg.Group("/file")
	{
		cont := &admin.File{}
		aR.POST("/notify", cont.Notify)
		aR.OPTIONS("/oss_token", nil)
		aR.OPTIONS("/upload", nil)
		aR.GET("/oss_token", cont.OssToken)
		aR.POST("/upload", cont.Upload)
	}
}*/

func MyBind(rg *gin.RouterGroup) {
	{
		cont := &my.ShareRecord{}
		rg.GET("/my/share_record/list", cont.List)
		rg.POST("/my/share_record/delete", cont.Delete)
		rg.POST("/my/share_record/batchDelete", cont.BatchDelete)
	}

	{
		cont := &my.AddressBook{}
		rg.GET("/my/address_book/list", cont.List)
		rg.POST("/my/address_book/create", cont.Create)
		rg.POST("/my/address_book/update", cont.Update)
		rg.POST("/my/address_book/delete", cont.Delete)
		rg.POST("/my/address_book/batchCreateFromPeers", cont.BatchCreateFromPeers)
		rg.POST("/my/address_book/batchUpdateTags", cont.BatchUpdateTags)
	}

	{
		cont := &my.Tag{}
		rg.GET("/my/tag/list", cont.List)
		rg.POST("/my/tag/create", cont.Create)
		rg.POST("/my/tag/update", cont.Update)
		rg.POST("/my/tag/delete", cont.Delete)
	}

	{
		cont := &my.AddressBookCollection{}
		rg.GET("/my/address_book_collection/list", cont.List)
		rg.POST("/my/address_book_collection/create", cont.Create)
		rg.POST("/my/address_book_collection/update", cont.Update)
		rg.POST("/my/address_book_collection/delete", cont.Delete)
	}

	{
		cont := &my.AddressBookCollectionRule{}
		rg.GET("/my/address_book_collection_rule/list", cont.List)
		rg.POST("/my/address_book_collection_rule/create", cont.Create)
		rg.POST("/my/address_book_collection_rule/update", cont.Update)
		rg.POST("/my/address_book_collection_rule/delete", cont.Delete)
	}
	{
		cont := &my.Peer{}
		rg.GET("/my/peer/list", cont.List)

	}

	{
		cont := &my.LoginLog{}
		rg.GET("/my/login_log/list", cont.List)
		rg.POST("/my/login_log/delete", cont.Delete)
		rg.POST("/my/login_log/batchDelete", cont.BatchDelete)
	}
}

func PeerStrategyBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/peer_strategy").Use(sessions.RequireAdmin())
	{
		cont := &admin.PeerStrategy{}
		aR.GET("/list", cont.List)
		aR.GET("/detail/:id", cont.Detail)
		aR.POST("/create", cont.Create)
		aR.POST("/update", cont.Update)
		aR.POST("/delete", cont.Delete)
		aR.GET("/default", cont.Default)
		aR.POST("/default/update", cont.UpdateDefault)
		aR.GET("/default_unbound", cont.DefaultUnbound)
		aR.POST("/default_unbound/update", cont.UpdateDefaultUnbound)
	}
}

func OperationLogBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/operation_log").Use(sessions.RequireAdmin())
	{
		cont := &admin.OperationLog{}
		aR.GET("/list", cont.List)
	}
}

func ShareRecordBind(rg *gin.RouterGroup, sessions *httpapi.Handler) {
	aR := rg.Group("/share_record").Use(sessions.RequireAdmin())
	{
		cont := &admin.ShareRecord{}
		aR.GET("/list", cont.List)
		aR.POST("/delete", cont.Delete)
		aR.POST("/batchDelete", cont.BatchDelete)
	}

}
