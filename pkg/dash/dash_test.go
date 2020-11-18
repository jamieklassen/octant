/*
 * Copyright (c) 2020 the Octant contributors. All Rights Reserved.
 * SPDX-License-Identifier: Apache-2.0
 *
 */

package dash

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/spf13/afero"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware-tanzu/octant/internal/api"
	ocontext "github.com/vmware-tanzu/octant/internal/context"
	"github.com/vmware-tanzu/octant/pkg/event"

	"github.com/vmware-tanzu/octant/internal/log"
)

func TestRunner_ValidateKubeconfig(t *testing.T) {
	fs := afero.NewMemMapFs()
	afero.WriteFile(fs, "/test1", []byte(""), 0755)
	afero.WriteFile(fs, "/test2", []byte(""), 0755)

	separator := string(filepath.ListSeparator)

	tests := []struct {
		name     string
		fileList string
		expected string
		isErr    bool
	}{
		{
			name:     "single path",
			fileList: "/test1",
			expected: "/test1",
			isErr:    false,
		},
		{
			name:     "multiple paths",
			fileList: "/test1" + separator + "/test2",
			expected: "/test1" + separator + "/test2",
			isErr:    false,
		},
		{
			name:     "single path not found",
			fileList: "/unknown",
			expected: "",
			isErr:    true,
		},
		{
			name:     "multiple paths not found",
			fileList: "/unknown" + separator + "/unknown2",
			expected: "",
			isErr:    true,
		},
		{
			name:     "multiple file path; missing a config",
			fileList: "/test1" + separator + "/unknown",
			expected: "/test1",
			isErr:    false,
		},
		{
			name:     "invalid path",
			fileList: "not a filepath",
			expected: "",
			isErr:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger := log.NopLogger()
			path, err := ValidateKubeConfig(logger, test.fileList, fs)
			if test.isErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, path, test.expected)
		})
	}
}

func TestNewRunnerLoadsValidKubeConfig(t *testing.T) {
	srv := fakeK8sAPIThatForbidsWatchingCRDs()
	defer srv.Close()
	kubeConfig := tempFile(fmt.Sprintf(`contexts:
- context: {cluster: cluster}
  name: test-context
clusters:
- cluster: {server: %s}
  name: cluster
current-context: test-context
`, srv.URL))
	defer os.Remove(kubeConfig.Name())
	stubRiceBox("dist/dash-frontend")

	addr := newListenerAddr()
	cancel, _ := makeRunner(Options{
		KubeConfig: kubeConfig.Name(),
	})
	defer cancel()
	kubeConfigEvent := waitForKubeConfigEvent(
		fmt.Sprintf("ws://%s/api/v1/stream", addr),
	)

	require.Equal(t, "test-context", kubeConfigEvent.Data.CurrentContext)
}

func TestNewRunnerRunsLoadingAPIWhenStartedWithoutKubeConfig(t *testing.T) {
	srv := fakeK8sAPIThatForbidsWatchingCRDs()
	defer srv.Close()
	stubRiceBox("dist/dash-frontend")

	addr := newListenerAddr()
	cancel, _ := makeRunner(Options{})
	defer cancel()
	conn, _, _ := websocket.DefaultDialer.Dial(
		fmt.Sprintf("ws://%s/api/v1/stream", addr),
		nil,
	)
	w, _ := conn.NextWriter(websocket.TextMessage)
	kubeConfig := fmt.Sprintf(`contexts:
- context: {cluster: cluster}
  name: test-context
clusters:
- cluster: {server: %s}
  name: cluster
current-context: test-context
`, srv.URL)
	encoded := base64.StdEncoding.EncodeToString([]byte(kubeConfig))
	w.Write([]byte(fmt.Sprintf(`{
	"type": "action.octant.dev/uploadKubeConfig",
	"payload": {"kubeConfig": "%s"}
}`, encoded)))
	w.Close()
}

// has side effect of changing global OCTANT_LISTENER_ADDR viper config
func newListenerAddr() string {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	viper.Set(api.ListenerAddrKey, addr)
	return addr
}

func waitForKubeConfigEvent(uri string) kubeConfigEvent {
	var message kubeConfigEvent
	conn, _, _ := websocket.DefaultDialer.Dial(uri, nil)
	for {
		msgBytes, _ := readNextMessage(conn)
		json.Unmarshal(msgBytes, &message)
		if message.Type == event.EventTypeKubeConfig {
			break
		}
	}
	return message
}

type kubeConfigEvent struct {
	Type event.EventType `json:"type"`
	Data struct {
		CurrentContext string `json:"currentContext"`
	} `json:"data"`
}

func tempFile(contents string) *os.File {
	tmpFile, _ := ioutil.TempFile("", "")
	tmpFile.Write([]byte(contents))
	tmpFile.Close()
	return tmpFile
}

func makeRunner(options Options) (context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = ocontext.WithKubeConfigCh(ctx)
	logger := log.NopLogger()
	runner, err := NewRunner(ctx, logger, options)
	if err != nil {
		return cancel, err
	}
	go runner.Start(ctx, logger, options, make(chan bool), make(chan bool))
	return cancel, nil
}

func fakeK8sAPIThatForbidsWatchingCRDs() *httptest.Server {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := httptest.NewUnstartedServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api":
				w.Write([]byte(fmt.Sprintf(`{
	"kind":"APIVersions",
	"versions":["v1"],
	"serverAddressByClientCIDRs": [
		{
			"clientCIDR": "0.0.0.0/0",
			"serverAddress": "%s"
		}
	]
}`, l.Addr().String())))
			case "/apis":
				w.Write([]byte(`{
	"kind": "APIGroupList",
	"apiVersion": "v1",
	"groups": [
		{
			"name": "apiextensions.k8s.io",
			"versions": [
				{
					"groupVersion": "apiextensions.k8s.io/v1beta1",
					"version": "v1beta1"
				}
			],
			"preferredVersion": {
				"groupVersion": "apiextensions.k8s.io/v1beta1",
				"version": "v1beta1"
			}
		}
	]
}`))
			case "/apis/apiextensions.k8s.io/v1beta1":
				w.Write([]byte(`{
	"kind": "APIResourceList",
	"apiVersion": "v1",
	"groupVersion": "apiextensions.k8s.io/v1beta1",
	"resources": [
		{
			"name": "customresourcedefinitions",
			"singularName": "",
			"namespaced": false,
			"kind": "CustomResourceDefinition",
			"verbs": [
				"create",
				"delete",
				"deletecollection",
				"get",
				"list",
				"patch",
				"update",
				"watch"
			],
			"shortNames": [
				"crd",
				"crds"
			],
			"storageVersionHash": "jfWCUB31mvA="
		},
		{
			"name": "customresourcedefinitions/status",
			"singularName": "",
			"namespaced": false,
			"kind": "CustomResourceDefinition",
			"verbs": [
				"get",
				"patch",
				"update"
			]
		}
	]
}`))
			case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
				w.Header().Add("Content-Type", "application/json")
				w.Write([]byte(`{
	"kind": "SelfSubjectAccessReview",
	"apiVersion": "authorization.k8s.io/v1",
	"metadata": {
		"creationTimestamp": null
	},
	"spec": {
		"resourceAttributes": {
			"verb": "watch"
		}
	},
	"status": {
		"allowed": false
	}
}`))
			}
		}),
	)
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	return srv
}

func stubRiceBox(name string) {
	_, callingGoFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(callingGoFile)
	os.MkdirAll(filepath.Join(pkgDir, "../../web", name), 0755)
}

func readNextMessage(conn *websocket.Conn) ([]byte, error) {
	_, reader, err := conn.NextReader()
	if err != nil {
		return nil, err
	}
	msgBytes, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return msgBytes, nil
}
