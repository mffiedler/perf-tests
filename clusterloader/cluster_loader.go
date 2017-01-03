/*
Copyright 2016 The Kubernetes Authors.

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

package clusterloader

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
	"unicode"

	"github.com/fatih/structs"
	clusterloaderframework "github.com/kubernetes/perf-tests/clusterloader/framework"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"k8s.io/kubernetes/pkg/api/v1"
	metav1 "k8s.io/kubernetes/pkg/apis/meta/v1"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/test/e2e/framework"
	testutils "k8s.io/kubernetes/test/utils"
)

type serviceInfo struct {
	Name string
	IP   string
	Port int32
}

var _ = framework.KubeDescribe("Cluster Loader [Feature:ManualPerformance]", func() {
	f := framework.NewDefaultFramework("cluster-loader")
	defer ginkgo.GinkgoRecover()

	var c clientset.Interface
	ginkgo.BeforeEach(func() {
		c = f.ClientSet
	})

	ginkgo.It(fmt.Sprintf("running config file"), func() {
		project := clusterloaderframework.ConfigContext.ClusterLoader.Projects
		tuningSets := clusterloaderframework.ConfigContext.ClusterLoader.TuningSets
		if len(project) < 1 {
			framework.Failf("invalid config file.\nFile: %v", project)
		}

		var namespaces []*v1.Namespace
		//totalPods := 0 // Keep track of how many pods for stepping
		// TODO sjug: add concurrency
		for _, p := range project {
			// Find tuning if we have it
			tuning := getTuningSet(tuningSets, p.Tuning)
			if tuning != nil {
				framework.Logf("Our tuning set is: %v", tuning)
			}
			for j := 0; j < p.Number; j++ {
				// Create namespaces as defined in Cluster Loader config
				nsName := appendIntToString(p.Basename, j)
				ns, err := f.CreateNamespace(nsName, nil)
				framework.ExpectNoError(err)
				framework.Logf("%d/%d : Created new namespace: %v", j+1, p.Number, nsName)
				namespaces = append(namespaces, ns)

				// Create templates as defined
				for _, v := range p.Templates {
					createTemplate(v.Basename, ns, mkPath(v.File), v.Number, tuning)
				}
				// This is too familiar, create pods
				for _, v := range p.Pods {
					// Parse Pod file into struct
					config := parsePods(mkPath(v.File))
					// Check if environment variables are defined in CL config
					if v.Parameters == (clusterloaderframework.ParameterConfigType{}) {
						framework.Logf("Pod environment variables will not be modified.")
					} else {
						// Override environment variables for Pod using ConfigMap
						configMapName := injectConfigMap(f, ns.Name, v.Parameters, config)
						// Cleanup ConfigMap at some point after the Pods are created
						defer func() {
							_ = f.ClientSet.Core().ConfigMaps(ns.Name).Delete(configMapName, nil)
						}()
					}
					labels := map[string]string{"purpose": "test"}
					clusterloaderframework.CreatePods(f, v.Basename, ns.Name, labels, config.Spec, v.Number, tuning)
				}
			}
		}

		// Wait for pods to be running
		for _, ns := range namespaces {
			label := labels.SelectorFromSet(labels.Set(map[string]string{"purpose": "test"}))
			err := testutils.WaitForPodsWithLabelRunning(c, ns.Name, label)
			if err != nil {
				framework.Failf("Got %v when trying to wait for the pods to start", err)
			}
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			framework.Logf("All pods running in namespace %s.", ns.Name)
		}

		//////////////////////////
		/// HTTP
		//////////////////////////
		// getIP
		//endpoints := getPodDetailsWithLabel(f, "purpose=test")
		////append endpoint
		//for _, endpointInfo := range endpoints {
		//	endpoint := "http://" + endpointInfo.IP + ":" + strconv.Itoa(int(endpointInfo.Port)) + "/ConsumeMem"
		//	//create url.Values from config
		//	m := structs.Map(parameters)
		//	values := url.Values{}
		//	for k, v := range m {
		//		k = firstLowercase(k)
		//		if v != 0 && v != "" {
		//			if _, ok := v.(int); ok {
		//				values.Add(k, strconv.Itoa(v.(int)))
		//			} else {
		//				values.Add(k, v.(string))
		//			}
		//		}
		//	}
		//	framework.Logf("HTTP post values: %+v", values)
		//	response, err := http.PostForm(endpoint, values)
		//	if err != nil {
		//		framework.Failf("HTTP request failed: %v", err)
		//	}
		//	responseData, err := ioutil.ReadAll(response.Body)
		//	if err != nil {
		//		framework.Failf("Error reading HTTP response: %v", err)
		//	}
		//	framework.Logf("Response from server: %v", string(responseData))
		//}
	})
})

func firstLowercase(s string) string {
	a := []rune(s)
	a[0] = unicode.ToLower(a[0])
	return string(a)
}

func convertVariables(params clusterloaderframework.ParameterConfigType) map[string]string {
	m := structs.Map(params)
	values := make(map[string]string)
	for k, v := range m {
		k = firstLowercase(k)
		if v != 0 && v != "" {
			if _, ok := v.(int); ok {
				values[k] = strconv.Itoa(v.(int))
			} else {
				values[k] = v.(string)
			}
		}
	}
	return values
}

func injectConfigMap(f *framework.Framework, ns string, vars clusterloaderframework.ParameterConfigType, config v1.Pod) string {
	configMapName := ns + "-configmap"
	configVars := convertVariables(vars)
	configMap := newConfigMap(ns, configMapName, configVars)
	framework.Logf("Creating configMap %v in namespace %v", configMap.Name, ns)
	var err error
	if configMap, err = f.ClientSet.Core().ConfigMaps(ns).Create(configMap); err != nil {
		framework.Failf("Unable to create test configMap %s: %v", configMap.Name, err)
	}

	for i, envVar := range config.Spec.Containers[0].Env {
		if _, ok := configVars[envVar.Name]; ok {
			framework.Logf("Found match to replace: %+v", envVar)
			config.Spec.Containers[0].Env[i] = v1.EnvVar{
				Name: envVar.Name,
				ValueFrom: &v1.EnvVarSource{
					ConfigMapKeyRef: &v1.ConfigMapKeySelector{
						LocalObjectReference: v1.LocalObjectReference{
							Name: configMapName,
						},
						Key: envVar.Name,
					},
				},
			}
		} else {
			framework.Logf("Environment variable %v is not defined in Pod file, skipping.", envVar.Name)
		}
	}
	return configMapName
}
func getEndpointsWithLabel(f *framework.Framework, label string) (endpointInfo []serviceInfo) {
	selector := v1.ListOptions{LabelSelector: label}
	endpoints, err := f.ClientSet.Core().Endpoints("").List(selector)
	if err != nil {
		panic(err.Error())
	}
	for _, v := range endpoints.Items {
		if len(v.Subsets) > 0 {
			for _, ep := range v.Subsets[0].Addresses {
				end := serviceInfo{v.ObjectMeta.Name, ep.IP, v.Subsets[0].Ports[0].Port}
				fmt.Printf("For endpoint \"%s\", the IP is %v, the port is %d\n", end.Name, end.IP, end.Port)
				endpointInfo = append(endpointInfo, end)
			}
		}
	}

	return
}

func getPodDetailsWithLabel(f *framework.Framework, label string) (podInfo []serviceInfo) {
	selector := v1.ListOptions{LabelSelector: label}
	pods, err := f.ClientSet.Core().Pods("").List(selector)
	if err != nil {
		panic(err.Error())
	}
	for _, v := range pods.Items {
		pod, err := f.ClientSet.Core().Pods(v.ObjectMeta.Namespace).Get(v.ObjectMeta.Name, metav1.GetOptions{})
		if err != nil {
			panic(err.Error())
		}
		info := serviceInfo{pod.Name, pod.Status.PodIP, pod.Spec.Containers[0].Ports[0].ContainerPort}
		fmt.Printf("For pod \"%s\", the IP is %v, the port is %d\n", info.Name, info.IP, info.Port)
		podInfo = append(podInfo, info)
	}

	return
}

// getTuningSet matches the name of the tuning set defined in the project and returns a pointer to the set
func getTuningSet(tuningSets []clusterloaderframework.TuningSetType, podTuning string) (tuning *clusterloaderframework.TuningSetType) {
	if podTuning != "" {
		// Iterate through defined tuningSets
		for _, ts := range tuningSets {
			// If we have a matching tuningSet keep it
			if ts.Name == podTuning {
				tuning = &ts
				return
			}
		}
		framework.Failf("No pod tuning found for: %s", podTuning)
	}
	return nil
}

// mkPath returns fully qualfied file location as a string
func mkPath(file string) string {
	// Handle an empty filename.
	if file == "" {
		framework.Failf("No template file defined!")
	}
	return filepath.Join("content/", file)
}

// createTemplate does regex substitution against the template file, then creates the template
func createTemplate(baseName string, ns *v1.Namespace, configPath string, numObjects int, tuning *clusterloaderframework.TuningSetType) {
	// Try to read the file
	content, err := ioutil.ReadFile(configPath)
	if err != nil {
		framework.Failf("Error reading file: %s", err)
	}

	// ${IDENTIFER} is what we're replacing in the file
	regex, err := regexp.Compile("\\${IDENTIFIER}")
	if err != nil {
		framework.Failf("Error compiling regex: %v", err)
	}

	for i := 0; i < numObjects; i++ {
		result := regex.ReplaceAll(content, []byte(strconv.Itoa(i)))

		tmpfile, err := ioutil.TempFile("", "cl")
		if err != nil {
			framework.Failf("Error creating new tempfile: %v", err)
		}
		defer os.Remove(tmpfile.Name())

		if _, err := tmpfile.Write(result); err != nil {
			framework.Failf("Error writing to tempfile: %v", err)
		}
		if err := tmpfile.Close(); err != nil {
			framework.Failf("Error closing tempfile: %v", err)
		}

		framework.RunKubectlOrDie("create", "-f", tmpfile.Name(), getNsCmdFlag(ns))
		framework.Logf("%d/%d : Created template %s", i+1, numObjects, baseName)

		// If there is a tuning set defined for this template
		if tuning != nil {
			if tuning.Templates.RateLimit.Delay != 0 {
				framework.Logf("Sleeping %d ms between template creation.", tuning.Templates.RateLimit.Delay)
				time.Sleep(time.Duration(tuning.Templates.RateLimit.Delay) * time.Millisecond)
			}
			if tuning.Templates.Stepping.StepSize != 0 && (i+1)%tuning.Templates.Stepping.StepSize == 0 {
				framework.Logf("We have created %d templates and are now sleeping for %d seconds", i+1, tuning.Templates.Stepping.Pause)
				time.Sleep(time.Duration(tuning.Templates.Stepping.Pause) * time.Second)
			}
		}
	}
}

// parsePods unmarshalls the json file defined in the CL config into a struct
func parsePods(jsonFile string) (configStruct v1.Pod) {
	configFile, err := ioutil.ReadFile(jsonFile)
	if err != nil {
		framework.Failf("Cant read config file. Error: %v", err)
	}

	err = json.Unmarshal(configFile, &configStruct)
	if err != nil {
		framework.Failf("Unable to unmarshal pod config. Error: %v", err)
	}

	framework.Logf("The loaded config file is: %+v", configStruct.Spec.Containers)
	return
}

func getNsCmdFlag(ns *v1.Namespace) string {
	return fmt.Sprintf("--namespace=%v", ns.Name)
}

func appendIntToString(s string, i int) string {
	return s + strconv.Itoa(i)
}

func newConfigMap(ns string, name string, vars map[string]string) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: v1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
		Data: vars,
	}
}
