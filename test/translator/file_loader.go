package translator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/kgateway-dev/kgateway/v2/internal/kgateway/translator/irtranslator"
	"github.com/kgateway-dev/kgateway/v2/pkg/utils/protoutils"

	"github.com/ghodss/yaml"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var NoFilesFound = errors.New("no k8s files found")

func LoadFromFiles(ctx context.Context, filename string, scheme *runtime.Scheme) ([]client.Object, error) {
	fileOrDir, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}

	var yamlFiles []string
	if fileOrDir.IsDir() {
		slog.Info("looking for YAML files", "path", fileOrDir.Name())
		err := filepath.WalkDir(filename, func(path string, d fs.DirEntry, _ error) error {
			if strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml") {
				yamlFiles = append(yamlFiles, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		yamlFiles = append(yamlFiles, filename)
	}

	if len(yamlFiles) == 0 {
		return nil, NoFilesFound
	}

	slog.Info("user configuration YAML files found", "files", yamlFiles)

	var resources []client.Object
	for _, file := range yamlFiles {
		objs, err := parseFile(file, scheme)
		if err != nil {
			return nil, err
		}

		for _, obj := range objs {
			clientObj, ok := obj.(client.Object)
			if !ok {
				return nil, fmt.Errorf("cannot convert runtime.Object to client.Object: %+v", obj)
			}

			_, isGwc := clientObj.(*gwv1.GatewayClass)
			if !isGwc && clientObj.GetNamespace() == "" {
				// fill in default namespace
				clientObj.SetNamespace("default")
			}
			resources = append(resources, clientObj)
		}
	}

	return resources, nil
}

func parseFile(filename string, scheme *runtime.Scheme) ([]runtime.Object, error) {
	file, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	type metaOnly struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	}

	// Split into individual YAML documents
	resourceYamlStrings := bytes.Split(file, []byte("\n---\n"))

	// Create resources from YAML documents
	var genericResources []runtime.Object
	for _, objYaml := range resourceYamlStrings {
		// Skip empty documents
		if len(bytes.TrimSpace(objYaml)) == 0 {
			continue
		}

		var meta metaOnly
		if err := yaml.Unmarshal(objYaml, &meta); err != nil {
			slog.Warn("failed to parse resource metadata, skipping YAML document",
				"filename", filename,
				"data", truncateString(string(objYaml), 100),
			)
			continue
		}

		gvk := schema.FromAPIVersionAndKind(meta.APIVersion, meta.Kind)
		obj, err := scheme.New(gvk)
		if err != nil {
			slog.Warn("unknown resource kind",
				"filename", filename,
				"gvk", gvk.String(),
				"data", truncateString(string(objYaml), 100),
			)
			continue
		}
		if err := yaml.Unmarshal(objYaml, obj); err != nil {
			slog.Warn("failed to parse resource YAML",
				"error", err,
				"filename", filename,
				"gvk", gvk.String(),
				"resource_id", obj.(client.Object).GetName()+"."+obj.(client.Object).GetNamespace(),
				"data", truncateString(string(objYaml), 100),
			)
			continue
		}

		genericResources = append(genericResources, obj)
	}

	return genericResources, err
}

func truncateString(str string, num int) string {
	result := str
	if len(str) > num {
		result = str[0:num] + "..."
	}
	return result
}

func ReadProxyFromFile(filename string) (*irtranslator.TranslationResult, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("reading proxy file: %w", err)
	}
	var proxy irtranslator.TranslationResult

	if err := UnmarshalAnyYaml(data, &proxy); err != nil {
		return nil, fmt.Errorf("parsing proxy from file: %w", err)
	}
	return &proxy, nil
}

func MarshalYaml(m proto.Message) ([]byte, error) {
	jsn, err := protoutils.MarshalBytes(m)
	if err != nil {
		return nil, err
	}
	return yaml.JSONToYAML(jsn)
}

func MarshalAnyYaml(m any) ([]byte, error) {
	jsn, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return yaml.JSONToYAML(jsn)
}

func UnmarshalAnyYaml(data []byte, into any) error {
	jsn, err := yaml.YAMLToJSON(data)
	if err != nil {
		return err
	}

	return json.Unmarshal(jsn, into)
}
