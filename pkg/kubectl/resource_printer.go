/*
Copyright 2014 Google Inc. All rights reserved.

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

package kubectl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/latest"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/golang/glog"
	"gopkg.in/v1/yaml"
)

// GetPrinter returns a resource printer and a bool indicating whether the object must be
// versioned for the given format.
func GetPrinter(version, format, templateFile string, defaultPrinter ResourcePrinter) (ResourcePrinter, error) {
	var printer ResourcePrinter
	switch format {
	case "json":
		printer = &JSONPrinter{version}
	case "yaml":
		printer = &YAMLPrinter{version}
	case "template":
		if len(templateFile) == 0 {
			return nil, fmt.Errorf("template format specified but no template given")
		}
		var err error
		printer, err = NewTemplatePrinter(version, []byte(templateFile))
		if err != nil {
			return nil, fmt.Errorf("error parsing template %s, %v\n", templateFile, err)
		}
	case "templatefile":
		if len(templateFile) == 0 {
			return nil, fmt.Errorf("templatefile format specified but no template file given")
		}
		data, err := ioutil.ReadFile(templateFile)
		if err != nil {
			return nil, fmt.Errorf("error reading template %s, %v\n", templateFile, err)
		}
		printer, err = NewTemplatePrinter(version, data)
		if err != nil {
			return nil, fmt.Errorf("error parsing template %s, %v\n", string(data), err)
		}
	case "":
		printer = defaultPrinter
	default:
		return nil, fmt.Errorf("output format %q not recognized", format)
	}
	return printer, nil
}

// ResourcePrinter is an interface that knows how to print API resources.
type ResourcePrinter interface {
	// Print receives an arbitrary object, formats it and prints it to a writer.
	PrintObj(runtime.Object, io.Writer) error

	// Returns true if this printer emits properly versioned output.
	IsVersioned() bool
}

// JSONPrinter is an implementation of ResourcePrinter which prints as JSON.
type JSONPrinter struct {
	version string
}

// PrintObj is an implementation of ResourcePrinter.PrintObj which simply writes the object to the Writer.
func (j *JSONPrinter) PrintObj(obj runtime.Object, w io.Writer) error {
	vi, err := latest.InterfacesFor(j.version)
	if err != nil {
		return err
	}

	data, err := vi.Codec.Encode(obj)
	if err != nil {
		return err
	}
	dst := bytes.Buffer{}
	err = json.Indent(&dst, data, "", "    ")
	dst.WriteByte('\n')
	_, err = w.Write(dst.Bytes())
	return err
}

// IsVersioned returns true.
func (*JSONPrinter) IsVersioned() bool { return true }

func toVersionedMap(version string, obj runtime.Object) (map[string]interface{}, error) {
	vi, err := latest.InterfacesFor(version)
	if err != nil {
		return nil, err
	}

	data, err := vi.Codec.Encode(obj)
	if err != nil {
		return nil, err
	}
	outObj := map[string]interface{}{}
	err = json.Unmarshal(data, &outObj)
	if err != nil {
		return nil, err
	}
	return outObj, nil
}

// YAMLPrinter is an implementation of ResourcePrinter which prints as YAML.
type YAMLPrinter struct {
	version string
}

// PrintObj prints the data as YAML.
func (y *YAMLPrinter) PrintObj(obj runtime.Object, w io.Writer) error {
	outObj, err := toVersionedMap(y.version, obj)
	if err != nil {
		return err
	}

	output, err := yaml.Marshal(outObj)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(w, string(output))
	return err
}

// IsVersioned returns true.
func (*YAMLPrinter) IsVersioned() bool { return true }

type handlerEntry struct {
	columns   []string
	printFunc reflect.Value
}

// HumanReadablePrinter is an implementation of ResourcePrinter which attempts to provide
// more elegant output. It is not threadsafe, but you may call PrintObj repeatedly; headers
// will only be printed if the object type changes. This makes it useful for printing items
// recieved from watches.
type HumanReadablePrinter struct {
	handlerMap map[reflect.Type]*handlerEntry
	noHeaders  bool
	lastType   reflect.Type
}

// IsVersioned returns false-- human readable printers do not make versioned output.
func (*HumanReadablePrinter) IsVersioned() bool { return false }

// NewHumanReadablePrinter creates a HumanReadablePrinter.
func NewHumanReadablePrinter(noHeaders bool) *HumanReadablePrinter {
	printer := &HumanReadablePrinter{
		handlerMap: make(map[reflect.Type]*handlerEntry),
		noHeaders:  noHeaders,
	}
	printer.addDefaultHandlers()
	return printer
}

// Handler adds a print handler with a given set of columns to HumanReadablePrinter instance.
// printFunc is the function that will be called to print an object.
// It must be of the following type:
//  func printFunc(object ObjectType, w io.Writer) error
// where ObjectType is the type of the object that will be printed.
func (h *HumanReadablePrinter) Handler(columns []string, printFunc interface{}) error {
	printFuncValue := reflect.ValueOf(printFunc)
	if err := h.validatePrintHandlerFunc(printFuncValue); err != nil {
		glog.Errorf("Unable to add print handler: %v", err)
		return err
	}
	objType := printFuncValue.Type().In(0)
	h.handlerMap[objType] = &handlerEntry{
		columns:   columns,
		printFunc: printFuncValue,
	}
	return nil
}

func (h *HumanReadablePrinter) validatePrintHandlerFunc(printFunc reflect.Value) error {
	if printFunc.Kind() != reflect.Func {
		return fmt.Errorf("invalid print handler. %#v is not a function.", printFunc)
	}
	funcType := printFunc.Type()
	if funcType.NumIn() != 2 || funcType.NumOut() != 1 {
		return fmt.Errorf("invalid print handler." +
			"Must accept 2 parameters and return 1 value.")
	}
	if funcType.In(1) != reflect.TypeOf((*io.Writer)(nil)).Elem() ||
		funcType.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
		return fmt.Errorf("invalid print handler. The expected signature is: "+
			"func handler(obj %v, w io.Writer) error", funcType.In(0))
	}
	return nil
}

var podColumns = []string{"NAME", "IMAGE(S)", "HOST", "LABELS", "STATUS"}
var replicationControllerColumns = []string{"NAME", "IMAGE(S)", "SELECTOR", "REPLICAS"}
var serviceColumns = []string{"NAME", "LABELS", "SELECTOR", "IP", "PORT"}
var minionColumns = []string{"NAME"}
var statusColumns = []string{"STATUS"}
var eventColumns = []string{"NAME", "KIND", "STATUS", "REASON", "MESSAGE"}

// addDefaultHandlers adds print handlers for default Kubernetes types.
func (h *HumanReadablePrinter) addDefaultHandlers() {
	h.Handler(podColumns, printPod)
	h.Handler(podColumns, printPodList)
	h.Handler(replicationControllerColumns, printReplicationController)
	h.Handler(replicationControllerColumns, printReplicationControllerList)
	h.Handler(serviceColumns, printService)
	h.Handler(serviceColumns, printServiceList)
	h.Handler(minionColumns, printMinion)
	h.Handler(minionColumns, printMinionList)
	h.Handler(statusColumns, printStatus)
	h.Handler(eventColumns, printEvent)
	h.Handler(eventColumns, printEventList)
}

func (h *HumanReadablePrinter) unknown(data []byte, w io.Writer) error {
	_, err := fmt.Fprintf(w, "Unknown object: %s", string(data))
	return err
}

func (h *HumanReadablePrinter) printHeader(columnNames []string, w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s\n", strings.Join(columnNames, "\t")); err != nil {
		return err
	}
	return nil
}

func podHostString(host, ip string) string {
	if host == "" && ip == "" {
		return "<unassigned>"
	}
	return host + "/" + ip
}

func printPod(pod *api.Pod, w io.Writer) error {
	// TODO: remove me when pods are converted
	spec := &api.PodSpec{}
	if err := api.Scheme.Convert(&pod.DesiredState.Manifest, spec); err != nil {
		glog.Errorf("Unable to convert pod manifest: %v", err)
	}

	_, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
		pod.Name, makeImageList(spec),
		podHostString(pod.CurrentState.Host, pod.CurrentState.HostIP),
		labels.Set(pod.Labels), pod.CurrentState.Status)
	return err
}

func printPodList(podList *api.PodList, w io.Writer) error {
	for _, pod := range podList.Items {
		if err := printPod(&pod, w); err != nil {
			return err
		}
	}
	return nil
}

func printReplicationController(controller *api.ReplicationController, w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s\t%s\t%s\t%d\n",
		controller.Name, makeImageList(&controller.Spec.Template.Spec),
		labels.Set(controller.Spec.Selector), controller.Spec.Replicas)
	return err
}

func printReplicationControllerList(list *api.ReplicationControllerList, w io.Writer) error {
	for _, controller := range list.Items {
		if err := printReplicationController(&controller, w); err != nil {
			return err
		}
	}
	return nil
}

func printService(svc *api.Service, w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", svc.Name, labels.Set(svc.Labels),
		labels.Set(svc.Spec.Selector), svc.Spec.PortalIP, svc.Spec.Port)
	return err
}

func printServiceList(list *api.ServiceList, w io.Writer) error {
	for _, svc := range list.Items {
		if err := printService(&svc, w); err != nil {
			return err
		}
	}
	return nil
}

func printMinion(minion *api.Minion, w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s\n", minion.Name)
	return err
}

func printMinionList(list *api.MinionList, w io.Writer) error {
	for _, minion := range list.Items {
		if err := printMinion(&minion, w); err != nil {
			return err
		}
	}
	return nil
}

func printStatus(status *api.Status, w io.Writer) error {
	_, err := fmt.Fprintf(w, "%v\n", status.Status)
	return err
}

func printEvent(event *api.Event, w io.Writer) error {
	_, err := fmt.Fprintf(
		w, "%s\t%s\t%s\t%s\t%s\n",
		event.InvolvedObject.Name,
		event.InvolvedObject.Kind,
		event.Status,
		event.Reason,
		event.Message,
	)
	return err
}

func printEventList(list *api.EventList, w io.Writer) error {
	for i := range list.Items {
		if err := printEvent(&list.Items[i], w); err != nil {
			return err
		}
	}
	return nil
}

// PrintObj prints the obj in a human-friendly format according to the type of the obj.
func (h *HumanReadablePrinter) PrintObj(obj runtime.Object, output io.Writer) error {
	w := tabwriter.NewWriter(output, 20, 5, 3, ' ', 0)
	defer w.Flush()
	t := reflect.TypeOf(obj)
	if handler := h.handlerMap[t]; handler != nil {
		if !h.noHeaders && t != h.lastType {
			h.printHeader(handler.columns, w)
			h.lastType = t
		}
		args := []reflect.Value{reflect.ValueOf(obj), reflect.ValueOf(w)}
		resultValue := handler.printFunc.Call(args)[0]
		if resultValue.IsNil() {
			return nil
		} else {
			return resultValue.Interface().(error)
		}
	} else {
		return fmt.Errorf("error: unknown type %#v", obj)
	}
}

// TemplatePrinter is an implementation of ResourcePrinter which formats data with a Go Template.
type TemplatePrinter struct {
	version  string
	template *template.Template
}

func NewTemplatePrinter(version string, tmpl []byte) (*TemplatePrinter, error) {
	t, err := template.New("output").Parse(string(tmpl))
	if err != nil {
		return nil, err
	}
	return &TemplatePrinter{version, t}, nil
}

// IsVersioned returns true.
func (*TemplatePrinter) IsVersioned() bool { return true }

// PrintObj formats the obj with the Go Template.
func (t *TemplatePrinter) PrintObj(obj runtime.Object, w io.Writer) error {
	outObj, err := toVersionedMap(t.version, obj)
	if err != nil {
		return err
	}
	return t.template.Execute(w, outObj)
}

func tabbedString(f func(*tabwriter.Writer) error) (string, error) {
	out := new(tabwriter.Writer)
	b := make([]byte, 1024)
	buf := bytes.NewBuffer(b)
	out.Init(buf, 0, 8, 1, '\t', 0)

	err := f(out)
	if err != nil {
		return "", err
	}

	out.Flush()
	str := string(buf.String())
	return str, nil
}
