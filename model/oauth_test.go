package model

import "testing"

func TestPublicOIDCConfigurationDefaultsAndRejectsDowngrade(t *testing.T) {
	base := func() Oauth { return Oauth{OauthType: "oidc", Issuer: "https://idp.test", ClientId: "rustdesk"} }
	c := base()
	if err := c.FormatOauthInfo(); err != nil {
		t.Fatal(err)
	}
	if c.ClientSecret != "" || c.Scopes != "openid,profile" || c.PkceEnable == nil || !*c.PkceEnable || c.PkceMethod != "S256" || c.AutoRegister == nil || *c.AutoRegister {
		t.Fatal("unsafe OIDC defaults")
	}
	for _, change := range []func(*Oauth){
		func(c *Oauth) { c.ClientSecret = "placeholder" }, func(c *Oauth) { c.Scopes = "openid,email" },
		func(c *Oauth) { c.Scopes = "openid offline_access" }, func(c *Oauth) { c.PkceMethod = "plain" },
		func(c *Oauth) { no := false; c.PkceEnable = &no }, func(c *Oauth) { c.Issuer = "http://idp.test" },
		func(c *Oauth) { c.Issuer = "https://idp.test?issuer=other" }, func(c *Oauth) { c.ClientId = "" },
	} {
		c := base()
		change(&c)
		if c.FormatOauthInfo() == nil {
			t.Fatal("invalid OIDC configuration accepted")
		}
	}
	c = base()
	c.Scopes = " openid, profile\t"
	if err := c.FormatOauthInfo(); err != nil || c.Scopes != OIDC_DEFAULT_SCOPES {
		t.Fatal("normal scope separators rejected")
	}
}
