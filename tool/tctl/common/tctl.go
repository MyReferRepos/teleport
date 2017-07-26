/*
Copyright 2015-2017 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"fmt"
	"os"
	"strings"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/config"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/service"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/Sirupsen/logrus"
	"github.com/buger/goterm"
	"github.com/gravitational/kingpin"
	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"
)

// GlobalCLIFlags keeps the CLI flags that apply to all tctl commands
type GlobalCLIFlags struct {
	Debug        bool
	ConfigFile   string
	ConfigString string
}

// CLICommand interface must be implemented by every CLI command
//
// This allows OSS and Enterprise Teleport editions to plug their own
// implementations of different CLI commands into the common execution
// framework
//
type CLICommand interface {
	// Initialize allows a caller-defined command to plug itself into CLI
	// argument parsing
	Initialize(*kingpin.Application, *service.Config)

	// TryRun is executed after the CLI parsing is done. The command must
	// determine if selectedCommand belongs to it and return match=true
	TryRun(selectedCommand string, c *auth.TunClient) (match bool, err error)
}

func Run2(distribution string, commands []CLICommand) {
	utils.InitLogger(utils.LoggingForCLI, logrus.WarnLevel)

	// app is the command line parser
	app := utils.InitCLIParser("tctl", GlobalHelpString)

	// cfg (teleport auth server configuration) is going to be shared by all
	// commands
	cfg := service.MakeDefaultConfig()

	// each command will add itself to the CLI parser:
	for i := range commands {
		commands[i].Initialize(app, cfg)
	}

	// these global flags apply to all commands
	var ccf GlobalCLIFlags
	app.Flag("debug", "Enable verbose logging to stderr").
		Short('d').
		BoolVar(&ccf.Debug)
	app.Flag("config", fmt.Sprintf("Path to a configuration file [%v]", defaults.ConfigFilePath)).
		Short('c').
		ExistingFileVar(&ccf.ConfigFile)
	app.Flag("config-string",
		"Base64 encoded configuration string").Hidden().Envar(defaults.ConfigEnvar).StringVar(&ccf.ConfigString)

	// "version" command is always available:
	ver := app.Command("version", "Print the version.")
	app.HelpFlag.Short('h')

	// parse CLI commands+flags:
	selectedCmd, err := app.Parse(os.Args[1:])
	if err != nil {
		utils.FatalError(err)
	}

	// "version" command?
	if selectedCmd == ver.FullCommand() {
		utils.PrintVersion(distribution)
		return
	}

	// configure all commands with Teleport configuration (they share 'cfg')
	applyConfig(&ccf, cfg)

	// connect to the auth sever:
	client, err := connectToAuthService(cfg)
	if err != nil {
		utils.FatalError(err)
	}

	// execute whatever is selected:
	var match bool
	for _, c := range commands {
		match, err = c.TryRun(selectedCmd, client)
		if err != nil {
			utils.FatalError(err)
		}
		if match {
			break
		}
	}
}

// Run() is the same as 'make'. It helps to share the code between different
// "distributions" like OSS or Enterprise
//
// distribution: name of the Teleport distribution
func Run(distribution string) {
	utils.InitLogger(utils.LoggingForCLI, logrus.WarnLevel)
	app := utils.InitCLIParser("tctl", GlobalHelpString)

	// generate default tctl configuration:
	cfg := service.MakeDefaultConfig()
	cmdAuth := AuthCommand{config: cfg}
	cmdTokens := TokenCommand{config: cfg}
	cmdGet := GetCommand{config: cfg}
	cmdCreate := CreateCommand{config: cfg}
	cmdDelete := DeleteCommand{config: cfg}

	// define global flags:
	var ccf GlobalCLIFlags
	app.Flag("debug", "Enable verbose logging to stderr").
		Short('d').
		BoolVar(&ccf.Debug)
	app.Flag("config", fmt.Sprintf("Path to a configuration file [%v]", defaults.ConfigFilePath)).
		Short('c').
		ExistingFileVar(&ccf.ConfigFile)
	app.Flag("config-string",
		"Base64 encoded configuration string").Hidden().Envar(defaults.ConfigEnvar).StringVar(&ccf.ConfigString)

	// commands:
	ver := app.Command("version", "Print the version.")
	app.HelpFlag.Short('h')

	delete := app.Command("del", "Delete resources").Hidden()
	delete.Arg("resource", "Resource to delete").SetValue(&cmdDelete.ref)

	// get one or many resources in the system
	get := app.Command("get", "Get one or many objects in the system").Hidden()
	get.Arg("resource", "Resource type and name").SetValue(&cmdGet.ref)
	get.Flag("format", "Format output type, one of 'yaml', 'json' or 'text'").Default(formatText).StringVar(&cmdGet.format)
	get.Flag("namespace", "Namespace of the resources").Default(defaults.Namespace).StringVar(&cmdGet.namespace)
	get.Flag("with-secrets", "Include secrets in resources like certificate authorities or OIDC connectors").Default("false").BoolVar(&cmdGet.withSecrets)

	// upsert one or many resources
	create := app.Command("create", "Create or update a resource").Hidden()
	create.Flag("filename", "resource definition file").Short('f').StringVar(&cmdCreate.filename)

	// operations on invitation tokens
	tokens := app.Command("tokens", "List or revoke invitation tokens")
	tokenList := tokens.Command("ls", "List node and user invitation tokens")
	tokenDel := tokens.Command("del", "Delete/revoke an invitation token")
	tokenDel.Arg("token", "Token to delete").StringVar(&cmdTokens.token)

	// operations with authorities
	auth := app.Command("auth", "Operations with user and host certificate authorities (CAs)").Hidden()
	authExport := auth.Command("export", "Export public cluster (CA) keys to stdout")
	authExport.Flag("keys", "if set, will print private keys").BoolVar(&cmdAuth.exportPrivateKeys)
	authExport.Flag("fingerprint", "filter authority by fingerprint").StringVar(&cmdAuth.exportAuthorityFingerprint)
	authExport.Flag("compat", "export cerfiticates compatible with specific version of Teleport").StringVar(&cmdAuth.compatVersion)
	authExport.Flag("type", "certificate type: 'user' or 'host'").StringVar(&cmdAuth.authType)

	authGenerate := auth.Command("gen", "Generate a new SSH keypair")
	authGenerate.Flag("pub-key", "path to the public key").Required().StringVar(&cmdAuth.genPubPath)
	authGenerate.Flag("priv-key", "path to the private key").Required().StringVar(&cmdAuth.genPrivPath)

	authSign := auth.Command("sign", "Create an identity file(s) for a given user")
	authSign.Flag("user", "Teleport user name").Required().StringVar(&cmdAuth.genUser)
	authSign.Flag("out", "identity output").Short('o').StringVar(&cmdAuth.output)
	authSign.Flag("format", "identity format: 'file' (default) or 'dir'").Default(string(client.DefaultIdentityFormat)).StringVar((*string)(&cmdAuth.outputFormat))
	authSign.Flag("ttl", "TTL (time to live) for the generated certificate").Default(fmt.Sprintf("%v", defaults.CertDuration)).DurationVar(&cmdAuth.genTTL)
	authSign.Flag("compat", "OpenSSH compatibility flag").StringVar(&cmdAuth.compatibility)

	// parse CLI commands+flags:
	command, err := app.Parse(os.Args[1:])
	if err != nil {
		utils.FatalError(err)
	}

	// "version" command?
	if command == ver.FullCommand() {
		utils.PrintVersion(distribution)
		return
	}

	applyConfig(&ccf, cfg)

	// some commands do not need a connection to client
	switch command {
	case authGenerate.FullCommand():
		err = cmdAuth.GenerateKeys()
		if err != nil {
			utils.FatalError(err)
		}
		return
	}
	// connect to the teleport auth service:
	client, err := connectToAuthService(cfg)
	if err != nil {
		utils.FatalError(err)
	}

	// execute the selected command:
	switch command {
	case get.FullCommand():
		err = cmdGet.Get(client)
	case create.FullCommand():
		err = cmdCreate.Create(client)
	case delete.FullCommand():
		err = cmdDelete.Delete(client)

	case authExport.FullCommand():
		err = cmdAuth.ExportAuthorities(client)
	case tokenList.FullCommand():
		err = cmdTokens.List(client)
	case tokenDel.FullCommand():
		err = cmdTokens.Del(client)
	case authSign.FullCommand():
		err = cmdAuth.GenerateAndSignKeys(client)
	}
	if err != nil {
		logrus.Error(err)
		utils.FatalError(err)
	}
}

// printHeader helper prints an ASCII table header to stdout
func printHeader(t *goterm.Table, cols []string) {
	dots := make([]string, len(cols))
	for i := range dots {
		dots[i] = strings.Repeat("-", len(cols[i]))
	}
	fmt.Fprint(t, strings.Join(cols, "\t")+"\n")
	fmt.Fprint(t, strings.Join(dots, "\t")+"\n")
}

// connectToAuthService creates a valid client connection to the auth service
func connectToAuthService(cfg *service.Config) (client *auth.TunClient, err error) {
	// connect to the local auth server by default:
	cfg.Auth.Enabled = true
	if len(cfg.AuthServers) == 0 {
		cfg.AuthServers = []utils.NetAddr{
			*defaults.AuthConnectAddr(),
		}
	}
	// read the host SSH keys and use them to open an SSH connection to the auth service
	i, err := auth.ReadIdentity(cfg.DataDir, auth.IdentityID{Role: teleport.RoleAdmin, HostUUID: cfg.HostUUID})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	client, err = auth.NewTunClient(
		"tctl",
		cfg.AuthServers,
		cfg.HostUUID,
		[]ssh.AuthMethod{ssh.PublicKeys(i.KeySigner)})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// check connectivity by calling something on a clinet:
	_, err = client.GetDialer()()
	if err != nil {
		utils.Consolef(os.Stderr,
			"Cannot connect to the auth server: %v.\nIs the auth server running on %v?", err, cfg.AuthServers[0].Addr)
		os.Exit(1)
	}
	return client, nil
}

// applyConfig takes configuration values from the config file and applies
// them to 'service.Config' object
func applyConfig(ccf *GlobalCLIFlags, cfg *service.Config) error {
	// load /etc/teleport.yaml and apply it's values:
	fileConf, err := config.ReadConfigFile(ccf.ConfigFile)
	if err != nil {
		return trace.Wrap(err)
	}
	// if configuration is passed as an environment variable,
	// try to decode it and override the config file
	if ccf.ConfigString != "" {
		fileConf, err = config.ReadFromString(ccf.ConfigString)
		if err != nil {
			return trace.Wrap(err)
		}
	}
	if err = config.ApplyFileConfig(fileConf, cfg); err != nil {
		return trace.Wrap(err)
	}
	// --debug flag
	if ccf.Debug {
		utils.InitLogger(utils.LoggingForCLI, logrus.DebugLevel)
		logrus.Debugf("DEBUG loggign enabled")
	}

	// read a host UUID for this node
	cfg.HostUUID, err = utils.ReadHostUUID(cfg.DataDir)
	if err != nil {
		utils.FatalError(err)
	}
	return nil
}
