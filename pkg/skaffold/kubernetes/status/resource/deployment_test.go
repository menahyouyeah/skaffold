/*
Copyright 2019 The Skaffold Authors

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

package resource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/testing/protocmp"

	"github.com/GoogleContainerTools/skaffold/v2/pkg/diag/validator"
	"github.com/GoogleContainerTools/skaffold/v2/pkg/skaffold/runner/runcontext"
	"github.com/GoogleContainerTools/skaffold/v2/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/v2/pkg/skaffold/util"
	"github.com/GoogleContainerTools/skaffold/v2/proto/v1"
	"github.com/GoogleContainerTools/skaffold/v2/testutil"
	testEvent "github.com/GoogleContainerTools/skaffold/v2/testutil/event"
)

func TestDeploymentCheckStatus(t *testing.T) {
	rolloutCmd := "kubectl --context kubecontext rollout status deployment graph --namespace test --watch=false"
	tests := []struct {
		description     string
		commands        util.Command
		expectedErr     string
		expectedDetails string
		cancelled       bool
		complete        bool
	}{
		{
			description: "rollout status success",
			commands: testutil.CmdRunOut(
				rolloutCmd,
				"deployment \"graph\" successfully rolled out",
			),
			expectedDetails: "successfully rolled out",
			complete:        true,
		},
		{
			description: "resource not complete",
			commands: testutil.CmdRunOut(
				rolloutCmd,
				"Waiting for replicas to be available",
			),
			expectedDetails: "waiting for replicas to be available",
		},
		{
			description: "no output",
			commands: testutil.CmdRunOut(
				rolloutCmd,
				"",
			),
		},
		{
			description: "rollout status error",
			commands: testutil.CmdRunOutErr(
				rolloutCmd,
				"",
				errors.New("error"),
			),
			expectedErr: "error",
			complete:    true,
		},
		{
			description: "rollout kubectl client connection error",
			commands: testutil.CmdRunOutErr(
				rolloutCmd,
				"",
				errors.New("Unable to connect to the server"),
			),
			expectedErr: MsgKubectlConnection,
		},
		{
			description: "set status to cancel",
			commands: testutil.CmdRunOutErr(
				rolloutCmd,
				"",
				errors.New("waiting for replicas to be available"),
			),
			cancelled:   true,
			complete:    true,
			expectedErr: "context cancelled",
		},
	}

	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			t.Override(&util.DefaultExecCommand, test.commands)
			testEvent.InitializeState([]latest.Pipeline{{}})

			r := NewResource("graph", ResourceTypes.Deployment, "test", 0, false)
			r.CheckStatus(context.Background(), &statusConfig{})

			if test.cancelled {
				r.UpdateStatus(&proto.ActionableErr{
					ErrCode: proto.StatusCode_STATUSCHECK_USER_CANCELLED,
					Message: "context cancelled",
				})
			}
			t.CheckDeepEqual(test.complete, r.IsStatusCheckCompleteOrCancelled())
			if test.expectedErr != "" {
				t.CheckErrorContains(test.expectedErr, r.Status().Error())
			} else {
				t.CheckDeepEqual(r.status.ae.Message, test.expectedDetails)
			}
		})
	}
}

func TestStandalonePodsCheckStatus(t *testing.T) {
	ns := "test-ns"
	tests := []struct {
		description     string
		podStatus       string
		expectedMessage string
		expectedErrCode proto.StatusCode
	}{
		{
			description:     "running pod not ready",
			podStatus:       "Running",
			expectedErrCode: proto.StatusCode_STATUSCHECK_STANDALONE_PODS_PENDING,
			expectedMessage: "pods not ready: [test-pod]",
		},
		{
			description:     "failed pod",
			podStatus:       "Failed",
			expectedErrCode: proto.StatusCode_STATUSCHECK_UNKNOWN,
			expectedMessage: "pod test-pod failed",
		},
		{
			description:     "pending pod",
			podStatus:       "Pending",
			expectedErrCode: proto.StatusCode_STATUSCHECK_STANDALONE_PODS_PENDING,
			expectedMessage: "pods not ready: [test-pod]",
		},
		{
			description:     "succeeded pod",
			podStatus:       "Succeeded",
			expectedErrCode: proto.StatusCode_STATUSCHECK_SUCCESS,
			expectedMessage: "",
		},
		{
			description:     "unknown status pod",
			podStatus:       "Unknown",
			expectedErrCode: proto.StatusCode_STATUSCHECK_STANDALONE_PODS_PENDING,
			expectedMessage: "pods not ready: [test-pod]",
		},
	}

	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			r := NewResource("test-standalone-pods", ResourceTypes.StandalonePods, ns, 42, false)
			r.resources = map[string]validator.Resource{}
			r.resources["test-pod"] = validator.NewResource(ns, "pod", "test-pod", validator.Status(test.podStatus), nil, nil)
			r.CheckStatus(context.Background(), &statusConfig{})
			t.CheckDeepEqual(r.status.ae.GetMessage(), test.expectedMessage)
			t.CheckDeepEqual(r.status.ae.GetErrCode(), test.expectedErrCode)
		})
	}
}

func TestParseKubectlError(t *testing.T) {
	tests := []struct {
		description string
		details     string
		err         error
		expectedAe  *proto.ActionableErr
	}{
		{
			description: "rollout status connection error",
			err:         errors.New("Unable to connect to the server"),
			expectedAe: &proto.ActionableErr{
				ErrCode: proto.StatusCode_STATUSCHECK_KUBECTL_CONNECTION_ERR,
				Message: MsgKubectlConnection,
			},
		},
		{
			description: "rollout status kubectl command killed",
			err:         errors.New("signal: killed"),
			expectedAe: &proto.ActionableErr{
				ErrCode: proto.StatusCode_STATUSCHECK_KUBECTL_PID_KILLED,
				Message: "received Ctrl-C or deployments could not stabilize within 10s: kubectl rollout status command interrupted\n",
			},
		},
		{
			description: "rollout status random error",
			err:         errors.New("deployment test not found"),
			expectedAe: &proto.ActionableErr{
				ErrCode: proto.StatusCode_STATUSCHECK_UNKNOWN,
				Message: "deployment test not found",
			},
		},
		{
			description: "rollout status nil error",
			details:     "successfully rolled out",
			expectedAe: &proto.ActionableErr{
				ErrCode: proto.StatusCode_STATUSCHECK_SUCCESS,
				Message: "successfully rolled out",
			},
		},
	}
	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			ae := parseKubectlRolloutError(test.details, 10*time.Second, false, test.err)
			t.CheckDeepEqual(test.expectedAe, ae, protocmp.Transform())
		})
	}
}

func TestIsErrAndNotRetriable(t *testing.T) {
	tests := []struct {
		description string
		statusCode  proto.StatusCode
		expected    bool
	}{
		{
			description: "rollout status connection error",
			statusCode:  proto.StatusCode_STATUSCHECK_KUBECTL_CONNECTION_ERR,
		},
		{
			description: "rollout status kubectl command killed",
			statusCode:  proto.StatusCode_STATUSCHECK_KUBECTL_PID_KILLED,
			expected:    true,
		},
		{
			description: "rollout status random error",
			statusCode:  proto.StatusCode_STATUSCHECK_UNKNOWN,
			expected:    true,
		},
		{
			description: "rollout status parent context canceled",
			statusCode:  proto.StatusCode_STATUSCHECK_USER_CANCELLED,
			expected:    true,
		},
		{
			description: "rollout status parent context timed out",
			statusCode:  proto.StatusCode_STATUSCHECK_DEADLINE_EXCEEDED,
			expected:    true,
		},
		{
			description: "unschedule error code",
			statusCode:  proto.StatusCode_STATUSCHECK_NODE_UNSCHEDULABLE,
		},
		{
			description: "unschedule unknown error",
			statusCode:  proto.StatusCode_STATUSCHECK_UNKNOWN_UNSCHEDULABLE,
		},
		{
			description: "rollout status nil error",
			statusCode:  proto.StatusCode_STATUSCHECK_SUCCESS,
			expected:    true,
		},
	}
	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			actual := isErrAndNotRetryAble(test.statusCode)
			t.CheckDeepEqual(test.expected, actual)
		})
	}
}

func TestReportSinceLastUpdated(t *testing.T) {
	tmpDir := filepath.Clean(os.TempDir())
	var tests = []struct {
		description  string
		ae           *proto.ActionableErr
		logs         []string
		expected     string
		expectedMute string
	}{
		{
			description: "logs more than 3 lines",
			ae:          &proto.ActionableErr{Message: "waiting for 0/1 deplotment to rollout\n"},
			logs: []string{
				"[pod container] Waiting for mongodb to start...",
				"[pod container] Waiting for connection for 2 sec",
				"[pod container] Retrying 1st attempt ....",
				"[pod container] Waiting for connection for 2 sec",
				"[pod container] Terminating with exit code 11",
			},
			expectedMute: fmt.Sprintf(` - test-ns:deployment/test: container terminated with exit code 11
    - test:pod/foo: container terminated with exit code 11
      > [pod container] Retrying 1st attempt ....
      > [pod container] Waiting for connection for 2 sec
      > [pod container] Terminating with exit code 11
      Full logs at %s
`, filepath.Join(tmpDir, "skaffold", "statuscheck", "foo.log")),
			expected: ` - test-ns:deployment/test: container terminated with exit code 11
    - test:pod/foo: container terminated with exit code 11
      > [pod container] Waiting for mongodb to start...
      > [pod container] Waiting for connection for 2 sec
      > [pod container] Retrying 1st attempt ....
      > [pod container] Waiting for connection for 2 sec
      > [pod container] Terminating with exit code 11
`,
		},
		{
			description: "logs less than 3 lines",
			ae:          &proto.ActionableErr{Message: "waiting for 0/1 deplotment to rollout\n"},
			logs: []string{
				"[pod container] Waiting for mongodb to start...",
				"[pod container] Waiting for connection for 2 sec",
				"[pod container] Terminating with exit code 11",
			},
			expected: ` - test-ns:deployment/test: container terminated with exit code 11
    - test:pod/foo: container terminated with exit code 11
      > [pod container] Waiting for mongodb to start...
      > [pod container] Waiting for connection for 2 sec
      > [pod container] Terminating with exit code 11
`,
			expectedMute: ` - test-ns:deployment/test: container terminated with exit code 11
    - test:pod/foo: container terminated with exit code 11
      > [pod container] Waiting for mongodb to start...
      > [pod container] Waiting for connection for 2 sec
      > [pod container] Terminating with exit code 11
`,
		},
		{
			description: "no logs or empty",
			ae:          &proto.ActionableErr{Message: "waiting for 0/1 deplotment to rollout\n"},
			expected: ` - test-ns:deployment/test: container terminated with exit code 11
    - test:pod/foo: container terminated with exit code 11
`,
			expectedMute: ` - test-ns:deployment/test: container terminated with exit code 11
    - test:pod/foo: container terminated with exit code 11
`,
		},
	}
	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			dep := NewResource("test", ResourceTypes.Deployment, "test-ns", 1, false)
			dep.resources = map[string]validator.Resource{
				"foo": validator.NewResource(
					"test",
					"pod",
					"foo",
					"Pending",
					&proto.ActionableErr{
						ErrCode: proto.StatusCode_STATUSCHECK_RUN_CONTAINER_ERR,
						Message: "container terminated with exit code 11"},
					test.logs,
				),
			}
			dep.UpdateStatus(test.ae)
			t.CheckDeepEqual(test.expectedMute, dep.ReportSinceLastUpdated(true))
			t.CheckTrue(dep.status.changed)
			// force report to false and Report again with mute logs false
			dep.status.reported = false
			t.CheckDeepEqual(test.expected, dep.ReportSinceLastUpdated(false))
		})
	}
}

func TestReportSinceLastUpdatedMultipleTimes(t *testing.T) {
	var tests = []struct {
		description     string
		podStatuses     []string
		reportStatusSeq []bool
		expected        string
	}{
		{
			description:     "report first time should return status",
			podStatuses:     []string{"cannot pull image"},
			reportStatusSeq: []bool{true},
			expected: ` - test-ns:deployment/test: cannot pull image
    - test:pod/foo: cannot pull image
`,
		},
		{
			description:     "report 2nd time should not return when same status",
			podStatuses:     []string{"cannot pull image", "cannot pull image"},
			reportStatusSeq: []bool{true, true},
			expected:        "",
		},
		{
			description:     "report called after multiple changes but last status was not changed.",
			podStatuses:     []string{"cannot pull image", "changed but not reported", "changed but not reported", "changed but not reported"},
			reportStatusSeq: []bool{true, false, false, true},
			expected: ` - test-ns:deployment/test: changed but not reported
    - test:pod/foo: changed but not reported
`,
		},
	}
	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			dep := NewResource("test", ResourceTypes.Deployment, "test-ns", 1, false)
			var actual string
			for i, status := range test.podStatuses {
				dep.UpdateStatus(&proto.ActionableErr{
					ErrCode: proto.StatusCode_STATUSCHECK_DEPLOYMENT_ROLLOUT_PENDING,
					Message: status,
				})
				dep.resources = map[string]validator.Resource{
					"foo": validator.NewResource(
						"test",
						"pod",
						"foo",
						"Pending",
						&proto.ActionableErr{
							ErrCode: proto.StatusCode_STATUSCHECK_DEPLOYMENT_ROLLOUT_PENDING,
							Message: status,
						},
						[]string{},
					),
				}
				if test.reportStatusSeq[i] {
					actual = dep.ReportSinceLastUpdated(false)
				}
			}
			t.CheckDeepEqual(test.expected, actual)
		})
	}
}

func TestStatusCode(t *testing.T) {
	var tests = []struct {
		description      string
		resourceStatuses []proto.StatusCode
		status           proto.StatusCode
		expected         proto.StatusCode
	}{
		{
			description: "user cancelled status returns correctly",
			status:      proto.StatusCode_STATUSCHECK_USER_CANCELLED,
			resourceStatuses: []proto.StatusCode{
				proto.StatusCode_STATUSCHECK_UNHEALTHY,
				proto.StatusCode_STATUSCHECK_SUCCESS,
			},
			expected: proto.StatusCode_STATUSCHECK_USER_CANCELLED,
		},
		{
			description: "successful returns correctly",
			status:      proto.StatusCode_STATUSCHECK_SUCCESS,
			resourceStatuses: []proto.StatusCode{
				proto.StatusCode_STATUSCHECK_CONTAINER_RESTARTING,
				proto.StatusCode_STATUSCHECK_SUCCESS,
			},
			expected: proto.StatusCode_STATUSCHECK_SUCCESS,
		},
		{
			description: "other dep status returns the pod status",
			status:      proto.StatusCode_STATUSCHECK_DEPLOYMENT_ROLLOUT_PENDING,
			resourceStatuses: []proto.StatusCode{
				proto.StatusCode_STATUSCHECK_CONTAINER_RESTARTING,
				proto.StatusCode_STATUSCHECK_SUCCESS,
			},
			expected: proto.StatusCode_STATUSCHECK_CONTAINER_RESTARTING,
		},
	}
	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			dep := NewResource("test", ResourceTypes.Deployment, "test-ns", 1, false)
			dep.UpdateStatus(&proto.ActionableErr{
				ErrCode: test.status,
				Message: "test status code",
			})
			dep.resources = map[string]validator.Resource{}
			for i, sc := range test.resourceStatuses {
				dep.resources[fmt.Sprintf("foo-%d", i)] = validator.NewResource(
					"test",
					"pod",
					"foo",
					"Pending",
					&proto.ActionableErr{
						ErrCode: sc,
						Message: "test status",
					},
					[]string{},
				)
			}
			t.CheckDeepEqual(test.expected, dep.StatusCode())
		})
	}
}

type statusConfig struct {
	runcontext.RunContext // Embedded to provide the default values.
}

func (c *statusConfig) GetKubeContext() string { return "kubecontext" }
