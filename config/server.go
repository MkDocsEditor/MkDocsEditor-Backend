package config

type ServerConfiguration struct {
	Port int
	Auth AuthenticationConfiguration
}

type AuthenticationConfiguration struct {
	User     string
	Password string
}