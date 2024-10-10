package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/rajatjindal/kubectl-modify-secret/pkg/editor"
	"github.com/rajatjindal/kubectl-modify-secret/pkg/secrets"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"

	//import all supported auth
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

// Version is set during build time
var Version = "unknown"

// ModifySecretOptions is struct for modify secret
type ModifySecretOptions struct {
	configFlags *genericclioptions.ConfigFlags
	IOStreams   genericclioptions.IOStreams

	args         []string
	kubeclient   kubernetes.Interface
	secretName   string
	namespace    string
	printVersion bool
}

// NewModifySecretOptions provides an instance of ModifySecretOptions with default values
func NewModifySecretOptions(streams genericclioptions.IOStreams) *ModifySecretOptions {
	return &ModifySecretOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:   streams,
	}
}

// NewCmdModifySecret provides a cobra command wrapping ModifySecretOptions
func NewCmdModifySecret(streams genericclioptions.IOStreams) *cobra.Command {
	o := NewModifySecretOptions(streams)

	cmd := &cobra.Command{
		Use:          "modify-secret [secret-name] [flags]",
		Short:        "Modify the secret with implicit base64 translations",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if o.printVersion {
				fmt.Println(Version)
				os.Exit(0)
			}

			if err := o.Complete(c, args); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			if err := o.Run(); err != nil {
				return err
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&o.printVersion, "version", false, "prints version of plugin")
	o.configFlags.AddFlags(cmd.Flags())

	return cmd
}

// Complete sets all information required for updating the current context
func (o *ModifySecretOptions) Complete(cmd *cobra.Command, args []string) error {
	o.args = args

	if len(args) > 0 {
		o.secretName = args[0]
	}

	config, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return err
	}

	o.kubeclient, err = kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	o.namespace = getNamespace(o.configFlags)
	return nil
}

// Validate ensures that all required arguments and flag values are provided
func (o *ModifySecretOptions) Validate() error {
	if len(o.args) == 0 {
		return fmt.Errorf("atleast one argument is required")
	}

	if len(o.args) > 1 {
		return fmt.Errorf("only one argument is allowed")
	}

	return nil
}

// Run fetches the given secret manifest from the cluster, decodes the payload, opens an editor to make changes, and applies the modified manifest when done
func (o *ModifySecretOptions) Run() error {
	secret, err := secrets.Get(context.TODO(), o.kubeclient, o.secretName, o.namespace)
	if err != nil {
		return err
	}

	data := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		decodedSecretLevel1, err := base64.StdEncoding.DecodeString(string(v))
		if err != nil {
			return fmt.Errorf("erreur lors du premier décodage base64 : %v", err)
		}
		decodedSecretLevel2, err := base64.StdEncoding.DecodeString(string(decodedSecretLevel1))
		if err != nil {
			return fmt.Errorf("erreur lors du deuxième décodage base64 : %v", err)
		}
		r, err := gzip.NewReader(bytes.NewReader(decodedSecretLevel2))
		if err != nil {
			return fmt.Errorf("erreur lors de la création du lecteur gzip : %v", err)
		}
		defer r.Close()

		decompressedSecret, err := ioutil.ReadAll(r)
		if err != nil {
			return fmt.Errorf("erreur lors de la décompression gzip : %v", err)
		}
		data[k] = string(decompressedSecret)

	}

	tempfile, err := os.CreateTemp("", fmt.Sprintf("%s-%s-*.yaml", o.namespace, o.secretName))
	if err != nil {
		return err
	}
	defer os.Remove(tempfile.Name())

	yamlData, err := yamlOrEmpty(data)
	if err != nil {
		return err
	}

	err = os.WriteFile(tempfile.Name(), yamlData, 0644)
	if err != nil {
		return err
	}

	originalSum := md5.Sum(yamlData)

	err = editor.Edit(tempfile.Name())
	if err != nil {
		return err
	}

	readData, err := ioutil.ReadFile(tempfile.Name())
	if err != nil {
		return err
	}

	finalSum := md5.Sum(readData)

	if originalSum == finalSum {
		logrus.Infof("no changes done to secret %q", o.secretName)
		return nil
	}

	var updateData map[string]string
	err = yaml.Unmarshal(readData, &updateData)
	if err != nil {
		return err
	}

	updateByteData := make(map[string][]byte, len(updateData))
	for k, v := range updateData {
		// 1. Compression gzip
		var buf bytes.Buffer
		gzipWriter := gzip.NewWriter(&buf)

		_, err = gzipWriter.Write([]byte(v))
		if err != nil {
			return fmt.Errorf("erreur lors de la compression gzip : %v", err)
		}

		err = gzipWriter.Close()
		if err != nil {
			return fmt.Errorf("erreur lors de la fermeture du writer gzip : %v", err)
		}

		compressedData := buf.Bytes()

		// 2. Premier encodage base64
		encodedSecretLevel1 := base64.StdEncoding.EncodeToString(compressedData)

		// 3. Deuxième encodage base64
		encodedSecretLevel2 := base64.StdEncoding.EncodeToString([]byte(encodedSecretLevel1))

		updateByteData[k] = []byte(encodedSecretLevel2)
	}

	secret.Data = updateByteData

	_, err = secrets.Update(context.TODO(), o.kubeclient, secret)
	if err != nil {
		return err
	}

	logrus.Infof("secret %q edited", o.secretName)

	return nil
}

// yamlOrEmpty renders a map to a YAML, with the exception that an empty map
// becomes an empty byte slice instead of []byte(`{}`)
func yamlOrEmpty(data map[string]string) ([]byte, error) {
	if len(data) == 0 {
		return []byte{}, nil
	}

	return yaml.Marshal(data)
}

// getNamespace takes a set of kubectl flag values and returns the namespace we should be operating in
func getNamespace(flags *genericclioptions.ConfigFlags) string {
	namespace, _, err := flags.ToRawKubeConfigLoader().Namespace()
	if err != nil || len(namespace) == 0 {
		namespace = "default"
	}
	return namespace
}
