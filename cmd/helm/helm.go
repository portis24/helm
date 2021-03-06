/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package main // import "k8s.io/helm/cmd/helm"

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
	"k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/client/restclient"

	"k8s.io/helm/pkg/helm"
	"k8s.io/helm/pkg/helm/helmpath"
	"k8s.io/helm/pkg/helm/portforwarder"
	"k8s.io/helm/pkg/kube"
	"k8s.io/helm/pkg/tiller/environment"
	"k8s.io/helm/pkg/tlsutil"
)

const (
	localRepoIndexFilePath = "index.yaml"
	homeEnvVar             = "HELM_HOME"
	hostEnvVar             = "HELM_HOST"
	tillerNamespaceEnvVar  = "TILLER_NAMESPACE"
)

var (
	tlsCaCertFile string // path to TLS CA certificate file
	tlsCertFile   string // path to TLS certificate file
	tlsKeyFile    string // path to TLS key file
	tlsVerify     bool   // enable TLS and verify remote certificates
	tlsEnable     bool   // enable TLS
)

var (
	helmHome        string
	tillerHost      string
	tillerNamespace string
	kubeContext     string
	// TODO refactor out this global var
	tillerTunnel *kube.Tunnel
)

// flagDebug is a signal that the user wants additional output.
var flagDebug bool

var globalUsage = `The Kubernetes package manager

To begin working with Helm, run the 'helm init' command:

	$ helm init

This will install Tiller to your running Kubernetes cluster.
It will also set up any necessary local configuration.

Common actions from this point include:

- helm search:    search for charts
- helm fetch:     download a chart to your local directory to view
- helm install:   upload the chart to Kubernetes
- helm list:      list releases of charts

Environment:
  $HELM_HOME          set an alternative location for Helm files. By default, these are stored in ~/.helm
  $HELM_HOST          set an alternative Tiller host. The format is host:port
  $HELM_NO_PLUGINS    disable plugins. Set HELM_NO_PLUGINS=1 to disable plugins.
  $TILLER_NAMESPACE   set an alternative Tiller namespace (default "kube-namespace")
  $KUBECONFIG         set an alternative Kubernetes configuration file (default "~/.kube/config")
`

func newRootCmd(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "helm",
		Short:        "The Helm package manager for Kubernetes.",
		Long:         globalUsage,
		SilenceUsage: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			tlsCaCertFile = os.ExpandEnv(tlsCaCertFile)
			tlsCertFile = os.ExpandEnv(tlsCertFile)
			tlsKeyFile = os.ExpandEnv(tlsKeyFile)
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			teardown()
		},
	}
	p := cmd.PersistentFlags()
	p.StringVar(&helmHome, "home", defaultHelmHome(), "location of your Helm config. Overrides $HELM_HOME")
	p.StringVar(&tillerHost, "host", defaultHelmHost(), "address of tiller. Overrides $HELM_HOST")
	p.StringVar(&kubeContext, "kube-context", "", "name of the kubeconfig context to use")
	p.BoolVar(&flagDebug, "debug", false, "enable verbose output")
	p.StringVar(&tillerNamespace, "tiller-namespace", defaultTillerNamespace(), "namespace of tiller")

	cmd.AddCommand(
		// chart commands
		newCreateCmd(out),
		newDependencyCmd(out),
		newFetchCmd(out),
		newInspectCmd(out),
		newLintCmd(out),
		newPackageCmd(out),
		newRepoCmd(out),
		newSearchCmd(out),
		newServeCmd(out),
		newVerifyCmd(out),

		// release commands
		addFlagsTLS(newDeleteCmd(nil, out)),
		addFlagsTLS(newGetCmd(nil, out)),
		addFlagsTLS(newHistoryCmd(nil, out)),
		addFlagsTLS(newInstallCmd(nil, out)),
		addFlagsTLS(newListCmd(nil, out)),
		addFlagsTLS(newRollbackCmd(nil, out)),
		addFlagsTLS(newStatusCmd(nil, out)),
		addFlagsTLS(newUpgradeCmd(nil, out)),

		addFlagsTLS(newReleaseTestCmd(nil, out)),
		addFlagsTLS(newResetCmd(nil, out)),
		addFlagsTLS(newVersionCmd(nil, out)),
		newCompletionCmd(out),
		newHomeCmd(out),
		newInitCmd(out),
		newResetCmd(nil, out),
		newVersionCmd(nil, out),
		newReleaseTestCmd(nil, out),
		newPluginCmd(out),

		// Hidden documentation generator command: 'helm docs'
		newDocsCmd(out),

		// Deprecated
		markDeprecated(newRepoUpdateCmd(out), "use 'helm repo update'\n"),
	)

	// Find and add plugins
	loadPlugins(cmd, helmpath.Home(homePath()), out)

	return cmd
}

func init() {
	// Tell gRPC not to log to console.
	grpclog.SetLogger(log.New(ioutil.Discard, "", log.LstdFlags))
}

func main() {
	cmd := newRootCmd(os.Stdout)
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func markDeprecated(cmd *cobra.Command, notice string) *cobra.Command {
	cmd.Deprecated = notice
	return cmd
}

func setupConnection(c *cobra.Command, args []string) error {
	if tillerHost == "" {
		config, client, err := getKubeClient(kubeContext)
		if err != nil {
			return err
		}

		tunnel, err := portforwarder.New(tillerNamespace, client, config)
		if err != nil {
			return err
		}

		tillerHost = fmt.Sprintf("localhost:%d", tunnel.Local)
		if flagDebug {
			fmt.Printf("Created tunnel using local port: '%d'\n", tunnel.Local)
		}
	}

	// Set up the gRPC config.
	if flagDebug {
		fmt.Printf("SERVER: %q\n", tillerHost)
	}
	// Plugin support.
	return nil
}

func teardown() {
	if tillerTunnel != nil {
		tillerTunnel.Close()
	}
}

func checkArgsLength(argsReceived int, requiredArgs ...string) error {
	expectedNum := len(requiredArgs)
	if argsReceived != expectedNum {
		arg := "arguments"
		if expectedNum == 1 {
			arg = "argument"
		}
		return fmt.Errorf("This command needs %v %s: %s", expectedNum, arg, strings.Join(requiredArgs, ", "))
	}
	return nil
}

// prettyError unwraps or rewrites certain errors to make them more user-friendly.
func prettyError(err error) error {
	if err == nil {
		return nil
	}
	// This is ridiculous. Why is 'grpc.rpcError' not exported? The least they
	// could do is throw an interface on the lib that would let us get back
	// the desc. Instead, we have to pass ALL errors through this.
	return errors.New(grpc.ErrorDesc(err))
}

func defaultHelmHome() string {
	if home := os.Getenv(homeEnvVar); home != "" {
		return home
	}
	return filepath.Join(os.Getenv("HOME"), ".helm")
}

func homePath() string {
	s := os.ExpandEnv(helmHome)
	os.Setenv(homeEnvVar, s)
	return s
}

func defaultHelmHost() string {
	return os.Getenv(hostEnvVar)
}

func defaultTillerNamespace() string {
	if ns := os.Getenv(tillerNamespaceEnvVar); ns != "" {
		return ns
	}
	return environment.DefaultTillerNamespace
}

// getKubeClient is a convenience method for creating kubernetes config and client
// for a given kubeconfig context
func getKubeClient(context string) (*restclient.Config, *internalclientset.Clientset, error) {
	config, err := kube.GetConfig(context).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("could not get kubernetes config for context '%s': %s", context, err)
	}
	client, err := internalclientset.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("could not get kubernetes client: %s", err)
	}
	return config, client, nil
}

// getKubeCmd is a convenience method for creating kubernetes cmd client
// for a given kubeconfig context
func getKubeCmd(context string) *kube.Client {
	return kube.New(kube.GetConfig(context))
}

// ensureHelmClient returns a new helm client impl. if h is not nil.
func ensureHelmClient(h helm.Interface) helm.Interface {
	if h != nil {
		return h
	}
	return newClient()
}

func newClient() helm.Interface {
	options := []helm.Option{helm.Host(tillerHost)}

	if tlsVerify || tlsEnable {
		tlsopts := tlsutil.Options{KeyFile: tlsKeyFile, CertFile: tlsCertFile, InsecureSkipVerify: true}
		if tlsVerify {
			tlsopts.CaCertFile = tlsCaCertFile
			tlsopts.InsecureSkipVerify = false
		}
		tlscfg, err := tlsutil.ClientConfig(tlsopts)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		options = append(options, helm.WithTLS(tlscfg))
	}
	return helm.NewClient(options...)
}

// addFlagsTLS adds the flags for supporting client side TLS to the
// helm command (only those that invoke communicate to Tiller.)
func addFlagsTLS(cmd *cobra.Command) *cobra.Command {
	// defaults
	var (
		tlsCaCertDefault = "$HELM_HOME/ca.pem"
		tlsCertDefault   = "$HELM_HOME/cert.pem"
		tlsKeyDefault    = "$HELM_HOME/key.pem"
	)

	// add flags
	cmd.Flags().StringVar(&tlsCaCertFile, "tls-ca-cert", tlsCaCertDefault, "path to TLS CA certificate file")
	cmd.Flags().StringVar(&tlsCertFile, "tls-cert", tlsCertDefault, "path to TLS certificate file")
	cmd.Flags().StringVar(&tlsKeyFile, "tls-key", tlsKeyDefault, "path to TLS key file")
	cmd.Flags().BoolVar(&tlsVerify, "tls-verify", false, "enable TLS for request and verify remote")
	cmd.Flags().BoolVar(&tlsEnable, "tls", false, "enable TLS for request")
	return cmd
}
