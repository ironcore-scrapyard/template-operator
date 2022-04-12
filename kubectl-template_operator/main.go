/*
 * Copyright (c) 2021 by the OnMetal authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/go-logr/logr"
	templatev1alpha1 "github.com/onmetal/template-operator/api/v1alpha1"
	"github.com/onmetal/template-operator/pkg/template"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	scheme = runtime.NewScheme()
	codec  runtime.Codec
	log    logr.Logger
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(templatev1alpha1.AddToScheme(scheme))

	codec = json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, json.SerializerOptions{
		Yaml:   true,
		Pretty: true,
	})
	log = zap.New(zap.UseDevMode(true))
}

func Command() *cobra.Command {
	var namespace string
	logOpts := &zap.Options{Development: true}
	cmd := &cobra.Command{
		Use: "template-operator",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			log = zap.New(zap.UseFlagOptions(logOpts))
			cmd.Context()
			return nil
		},
	}

	cmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", corev1.NamespaceDefault, "Namespace to use.")
	cmd.PersistentFlags().AddGoFlagSet(flag.CommandLine)
	logFs := flag.NewFlagSet("", flag.ExitOnError)
	logOpts.BindFlags(logFs)
	cmd.PersistentFlags().AddGoFlagSet(logFs)

	cmd.AddCommand(
		RenderCommand(&namespace),
	)

	return cmd
}

func RenderCommand(namespace *string) *cobra.Command {
	var filename string

	cmd := &cobra.Command{
		Use:  "render",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name string
			if len(args) > 0 {
				name = args[0]
			}
			ctx := cmd.Context()
			ctx = ctrl.LoggerInto(ctx, log)
			return RunRender(ctx, *namespace, name, filename)
		},
	}

	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Filename pointing to a template. '-' means stdin.")

	return cmd
}

func ReadFileOrStdin(filename string) ([]byte, error) {
	if filename == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(filename)
}

func readTemplate(namespace, filename string) (*templatev1alpha1.Template, error) {
	data, err := ReadFileOrStdin(filename)
	if err != nil {
		return nil, fmt.Errorf("error reading file %s: %w", filename, err)
	}

	tmpl := &templatev1alpha1.Template{}
	if _, _, err := codec.Decode(data, nil, tmpl); err != nil {
		return nil, fmt.Errorf("error reading template: %w", err)
	}

	if tmpl.Namespace != "" && tmpl.Namespace != namespace {
		return nil, fmt.Errorf("namespace mismatch: template specifies %s but given via flag is %s",
			tmpl.Namespace, namespace)
	}
	tmpl.Namespace = namespace
	return tmpl, nil
}

func getTemplate(ctx context.Context, c client.Client, namespace, name string) (*templatev1alpha1.Template, error) {
	key := client.ObjectKey{Namespace: namespace, Name: name}
	tmpl := &templatev1alpha1.Template{}
	if err := c.Get(ctx, key, tmpl); err != nil {
		return nil, fmt.Errorf("error getting template %s: %w", key, err)
	}

	return tmpl, nil
}

func getOrReadTemplate(ctx context.Context, c client.Client, namespace, name, filename string) (*templatev1alpha1.Template, error) {
	switch {
	case name == "" && filename == "":
		return nil, fmt.Errorf("either name or filename has to be specified")
	case name != "" && filename != "":
		return nil, fmt.Errorf("cannot specify both filename and name")
	case name != "":
		return getTemplate(ctx, c, namespace, name)
	case filename != "":
		return readTemplate(namespace, filename)
	default:
		panic("unhandled case")
	}
}

func RunRender(ctx context.Context, namespace, name, filename string) error {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("error obtaining config: %w", err)
	}

	c, err := client.New(cfg, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		return fmt.Errorf("error creating client: %w", err)
	}

	tmpl, err := getOrReadTemplate(ctx, c, namespace, name, filename)
	if err != nil {
		return fmt.Errorf("could not obtain template: %w", err)
	}

	objs, err := template.NewEngine(c, scheme).Render(ctx, tmpl)
	if err != nil {
		return fmt.Errorf("error rendering objects: %w", err)
	}

	for i, obj := range objs {
		if i > 0 {
			fmt.Println("---")
		}

		res, err := runtime.Encode(codec, &obj)
		if err != nil {
			return fmt.Errorf("error encoding object: %w", err)
		}

		fmt.Println(string(res))
	}
	return nil
}

func main() {
	if err := Command().Execute(); err != nil {
		log.Error(err, "Error running command.")
		os.Exit(1)
	}
}
