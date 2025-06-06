package autocli

import (
	"github.com/cosmos/gogoproto/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoregistry"

	autocliv1 "cosmossdk.io/api/cosmos/autocli/v1"
	"cosmossdk.io/client/v2/autocli/flag"
	"cosmossdk.io/core/address"
	"cosmossdk.io/core/appmodule"
	"cosmossdk.io/depinject"

	"github.com/cosmos/cosmos-sdk/client"
	sdkflags "github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/runtime"
)

// AppOptions are autocli options for an app. These options can be built via depinject based on an app config. Ex:
// Ex:
//
//	var autoCliOpts autocli.AppOptions
//	err := depinject.Inject(appConfig, &encodingConfig.InterfaceRegistry, &autoCliOpts)
//
// If depinject isn't used, options can be provided manually or extracted from modules and the address codec can be provided by the auth keeper.
// One method for extracting autocli options is via the github.com/cosmos/cosmos-sdk/runtime/services.ExtractAutoCLIOptions function.
type AppOptions struct {
	depinject.In

	// Modules are the AppModule implementations for the modules in the app.
	Modules map[string]appmodule.AppModule

	// ModuleOptions are autocli options to be used for modules instead of what
	// is specified on the module's AppModule implementation. This allows an
	// app to override module options if they are either not provided by a
	// module or need to be improved.
	ModuleOptions map[string]*autocliv1.ModuleOptions `optional:"true"`

	// AddressCodec is the address codec to use for the app.
	AddressCodec          address.Codec
	ValidatorAddressCodec runtime.ValidatorAddressCodec
	ConsensusAddressCodec runtime.ConsensusAddressCodec

	// ClientCtx contains the necessary information needed to execute the commands.
	ClientCtx client.Context
}

// EnhanceRootCommand enhances the provided root command with autocli AppOptions,
// only adding missing commands and doesn't override commands already
// in the root command. This allows for the graceful integration of autocli with
// existing app CLI commands where autocli simply automatically adds things that
// weren't manually provided. It does take into account custom commands
// provided by modules with the HasCustomQueryCommand or HasCustomTxCommand extension interface.
// Example Usage:
//
//	var autoCliOpts autocli.AppOptions
//	err := depinject.Inject(appConfig, &autoCliOpts)
//	if err != nil {
//		panic(err)
//	}
//	rootCmd := initRootCmd()
//	err = autoCliOpts.EnhanceRootCommand(rootCmd)
func (appOptions AppOptions) EnhanceRootCommand(rootCmd *cobra.Command) error {
	var (
		mergedFiles flag.FileResolver
		err         error
	)

	mergedFiles, err = proto.MergedRegistry()
	if err != nil {
		// we can safely ignore this error, as this should have been called somewhere earlier
		// in the app's lifecycle.
		mergedFiles = appOptions.ClientCtx.InterfaceRegistry
	}

	builder := &Builder{
		Builder: flag.Builder{
			TypeResolver:          protoregistry.GlobalTypes,
			FileResolver:          mergedFiles,
			AddressCodec:          appOptions.AddressCodec,
			ValidatorAddressCodec: appOptions.ValidatorAddressCodec,
			ConsensusAddressCodec: appOptions.ConsensusAddressCodec,
		},
		GetClientConn: func(cmd *cobra.Command) (grpc.ClientConnInterface, error) {
			return client.GetClientQueryContext(cmd)
		},
		AddQueryConnFlags: func(c *cobra.Command) {
			sdkflags.AddQueryFlagsToCmd(c)
			sdkflags.AddKeyringFlags(c.Flags())
		},
		AddTxConnFlags: sdkflags.AddTxFlagsToCmd,
	}

	return appOptions.EnhanceRootCommandWithBuilder(rootCmd, builder)
}

func (appOptions AppOptions) EnhanceRootCommandWithBuilder(rootCmd *cobra.Command, builder *Builder) error {
	if err := builder.ValidateAndComplete(); err != nil {
		return err
	}

	// extract any custom commands from modules
	customQueryCmds, customMsgCmds := map[string]*cobra.Command{}, map[string]*cobra.Command{}
	for name, module := range appOptions.Modules {
		if queryModule, ok := module.(HasCustomQueryCommand); ok {
			queryCmd := queryModule.GetQueryCmd()
			// filter any nil commands
			if queryCmd != nil {
				customQueryCmds[name] = queryCmd
			}
		}
		if msgModule, ok := module.(HasCustomTxCommand); ok {
			msgCmd := msgModule.GetTxCmd()
			// filter any nil commands
			if msgCmd != nil {
				customMsgCmds[name] = msgCmd
			}
		}
	}

	if queryCmd := findSubCommand(rootCmd, "query"); queryCmd != nil {
		if err := builder.enhanceCommandCommon(queryCmd, queryCmdType, appOptions, customQueryCmds); err != nil {
			return err
		}
	} else {
		queryCmd, err := builder.BuildQueryCommand(rootCmd.Context(), appOptions, customQueryCmds)
		if err != nil {
			return err
		}

		rootCmd.AddCommand(queryCmd)
	}

	if msgCmd := findSubCommand(rootCmd, "tx"); msgCmd != nil {
		if err := builder.enhanceCommandCommon(msgCmd, msgCmdType, appOptions, customMsgCmds); err != nil {
			return err
		}
	} else {
		subCmd, err := builder.BuildMsgCommand(rootCmd.Context(), appOptions, customMsgCmds)
		if err != nil {
			return err
		}

		rootCmd.AddCommand(subCmd)
	}

	return nil
}
