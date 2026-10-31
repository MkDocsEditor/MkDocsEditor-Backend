package configuration

type (
	ServerConfiguration struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
		// Auth configures the users that may access the backend
		Auth AuthConfiguration `yaml:"auth"`
		// BasicAuth is the old single-user configuration. It keeps working; the user is an admin.
		BasicAuth AuthenticationConfiguration `yaml:"basicAuth"`
		CORS      CorsConfiguration           `yaml:"cors"`
	}

	AuthConfiguration struct {
		Users []UserConfiguration `yaml:"users"`
	}

	UserConfiguration struct {
		Username string `yaml:"username"`
		// PasswordHash is a bcrypt hash, see "mkdocsrest hash-password"
		PasswordHash string `yaml:"passwordHash"`
		// DisplayName is shown to other users and used as the git author name (optional)
		DisplayName string `yaml:"displayName"`
		// Email is used as the git author email (optional)
		Email string `yaml:"email"`
		// Role is one of admin, editor, reader (default: editor)
		Role string `yaml:"role"`
		// Color overrides the colour derived from the username, e.g. "#1971c2" (optional)
		Color string `yaml:"color"`
	}

	AuthenticationConfiguration struct {
		User     string `yaml:"user"`
		Password string `yaml:"password"`
	}

	CorsConfiguration struct {
		AllowedOrigins []string `yaml:"allowedOrigins"`
		AllowedMethods []string `yaml:"allowedMethods"`
	}
)
