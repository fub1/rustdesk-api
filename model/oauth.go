package model

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

const OIDC_DEFAULT_SCOPES = "openid,profile"

const (
	// make sure the value shouldbe lowercase
	OauthTypeGithub  string = "github"
	OauthTypeGoogle  string = "google"
	OauthTypeOidc    string = "oidc"
	OauthTypeWebauth string = "webauth"
	OauthTypeLinuxdo string = "linuxdo"
	PKCEMethodS256   string = "S256"
	PKCEMethodPlain  string = "plain"
)

// Validate the oauth type
func ValidateOauthType(oauthType string) error {
	switch oauthType {
	case OauthTypeGithub, OauthTypeGoogle, OauthTypeOidc, OauthTypeWebauth, OauthTypeLinuxdo:
		return nil
	default:
		return errors.New("invalid Oauth type")
	}
}

const (
	UserEndpointGithub  string = "https://api.github.com/user"
	UserEndpointLinuxdo string = "https://connect.linux.do/api/user"
	IssuerGoogle        string = "https://accounts.google.com"
)

type Oauth struct {
	IdModel
	Op           string `json:"op"`
	OauthType    string `json:"oauth_type"`
	ClientId     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	//RedirectUrl  string `json:"redirect_url"`
	AutoRegister *bool  `json:"auto_register"`
	Scopes       string `json:"scopes"`
	Issuer       string `json:"issuer"`
	PkceEnable   *bool  `json:"pkce_enable"`
	PkceMethod   string `json:"pkce_method"`
	TimeModel
}

// Helper function to format oauth info, it's used in the update and create method
func (oa *Oauth) FormatOauthInfo() error {
	oauthType := strings.TrimSpace(oa.OauthType)
	err := ValidateOauthType(oa.OauthType)
	if err != nil {
		return err
	}
	if oauthType == OauthTypeOidc {
		u, err := url.Parse(oa.Issuer)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(oa.Issuer) > 512 || strings.TrimSpace(oa.ClientId) == "" || oa.ClientSecret != "" {
			return errors.New("OIDC requires an HTTPS issuer and a public client without secret")
		}
		for _, scope := range strings.FieldsFunc(oa.Scopes, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
			if scope != "openid" && scope != "profile" {
				return errors.New("OIDC supports only openid profile")
			}
		}
		if oa.PkceEnable != nil && !*oa.PkceEnable || oa.PkceMethod != "" && oa.PkceMethod != PKCEMethodS256 {
			return errors.New("OIDC requires S256 PKCE")
		}
		oa.Scopes = OIDC_DEFAULT_SCOPES
		enabled := true
		oa.PkceEnable = &enabled
		oa.PkceMethod = PKCEMethodS256
		if oa.AutoRegister == nil {
			closed := false
			oa.AutoRegister = &closed
		}
	}
	switch oauthType {
	case OauthTypeGithub:
		oa.Op = OauthTypeGithub
	case OauthTypeGoogle:
		oa.Op = OauthTypeGoogle
	case OauthTypeLinuxdo:
		oa.Op = OauthTypeLinuxdo
	}
	// check if the op is empty, set the default value
	op := strings.TrimSpace(oa.Op)
	if op == "" && oauthType == OauthTypeOidc {
		oa.Op = OauthTypeOidc
	}
	// check the issuer, if the oauth type is google and the issuer is empty, set the issuer to the default value
	issuer := strings.TrimSpace(oa.Issuer)
	// If the oauth type is google and the issuer is empty, set the issuer to the default value
	if oauthType == OauthTypeGoogle && issuer == "" {
		oa.Issuer = IssuerGoogle
	}
	if oa.PkceEnable == nil {
		oa.PkceEnable = new(bool)
		*oa.PkceEnable = false
	}
	if oa.PkceMethod == "" {
		oa.PkceMethod = PKCEMethodS256
	}
	return nil
}

type OauthUser struct {
	OpenId        string `json:"open_id" gorm:"not null;index"`
	Name          string `json:"name"`
	Username      string `json:"username"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email,omitempty"`
	Picture       string `json:"picture,omitempty"`
}

func (ou *OauthUser) ToUser(user *User, overideUsername bool) {
	if overideUsername {
		user.Username = ou.Username
	}
	user.Email = ou.Email
	user.Nickname = ou.Name
	user.Avatar = ou.Picture
}

type OauthUserBase struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type OidcUser struct {
	OauthUserBase
	Sub               string `json:"sub"`
	VerifiedEmail     bool   `json:"email_verified"`
	PreferredUsername string `json:"preferred_username"`
	Picture           string `json:"picture"`
}

func (ou *OidcUser) ToOauthUser() *OauthUser {
	var username string
	// 使用 PreferredUsername，如果不存在，降级到 Email 前缀
	if ou.PreferredUsername != "" {
		username = ou.PreferredUsername
	} else {
		username = strings.ToLower(ou.Email)
	}

	return &OauthUser{
		OpenId:        ou.Sub,
		Name:          ou.Name,
		Username:      username,
		Email:         ou.Email,
		VerifiedEmail: ou.VerifiedEmail,
		Picture:       ou.Picture,
	}
}

type GithubUser struct {
	OauthUserBase
	Id            int    `json:"id"`
	Login         string `json:"login"`
	AvatarUrl     string `json:"avatar_url"`
	VerifiedEmail bool   `json:"verified_email"`
}

func (gu *GithubUser) ToOauthUser() *OauthUser {
	username := strings.ToLower(gu.Login)
	return &OauthUser{
		OpenId:        strconv.Itoa(gu.Id),
		Name:          gu.Name,
		Username:      username,
		Email:         gu.Email,
		VerifiedEmail: gu.VerifiedEmail,
		Picture:       gu.AvatarUrl,
	}
}

type LinuxdoUser struct {
	OauthUserBase
	Id       int    `json:"id"`
	Username string `json:"username"`
	Avatar   string `json:"avatar_url"`
}

func (lu *LinuxdoUser) ToOauthUser() *OauthUser {
	return &OauthUser{
		OpenId:        strconv.Itoa(lu.Id),
		Name:          lu.Name,
		Username:      strings.ToLower(lu.Username),
		Email:         lu.Email,
		VerifiedEmail: true, // linux.do 用户邮箱默认已验证
		Picture:       lu.Avatar,
	}
}

type OauthList struct {
	Oauths []*Oauth `json:"list"`
	Pagination
}
