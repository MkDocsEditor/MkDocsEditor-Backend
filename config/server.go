package config

type ServerConfiguration struct {
	Host string
	Port int
	Auth AuthenticationConfiguration
}

type AuthenticationConfiguration struct {
	User     string
	Password string
}