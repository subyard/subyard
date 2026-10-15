package cli

import (
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/uninstall"
	"maps"
)

type fullTeardownService = uninstall.RecoveryService

func (cli *CLI) captureCoreServices(current config.Loaded, plan uninstall.RecoveryPlan, owners []string, port int) ([]fullTeardownService, error) {
	if cli.options.ObserveCoreProducers != nil {
		return cli.options.ObserveCoreProducers(current, plan, owners, port)
	}
	environment := maps.Clone(current.Environment)
	environment["SUBYARD_OPERATOR_HOME"] = current.Context.Paths.OperatorHome
	environment["SUBYARD_DISPATCHER_PATH"] = cli.options.DispatcherPath
	return uninstall.CaptureProducers(cli.options.RepositoryRoot, plan, environment, owners, port)
}
