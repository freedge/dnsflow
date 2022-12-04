package main

import (
	"context"
	"flag"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	dnstap "github.com/dnstap/golang-dnstap"
	"github.com/miekg/dns"
	selinux "github.com/opencontainers/selinux/go-selinux"
	v1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var mu sync.Mutex
var gnameToSourceIP map[string][]string

func allowTrafficIfNeeded(name string, ip string) {
	if strings.Count(ip, ".") != 3 || len(name) < 4 {
		return
	}
	mu.Lock()
	nameToSourceIP := gnameToSourceIP
	mu.Unlock()
	for key, sourceIPs := range nameToSourceIP {
		if !strings.HasSuffix(strings.TrimSuffix(name, "."), key) {
			continue
		}
		fmt.Printf("%s matches %s\n", name, key)
		// a name has been resolved, we add some flows
		for _, sourceIP := range sourceIPs {
			if strings.Count(sourceIP, ".") != 3 {
				continue
			}
			flow := fmt.Sprintf(
				"table=44,cookie=0xba5ed,priority=11000,ip,reg14=0x1,nw_src=%s,nw_dst=%s actions=resubmit(,45)",
				sourceIP, ip,
			)
			fmt.Printf("adding flow %s\n", flow)
			cmd := exec.Command("ovs-ofctl", "add-flow", "br-int", flow)
			stdout, err := cmd.Output()

			if err != nil {
				fmt.Println(err.Error())
				return
			}

			// Print the output
			fmt.Println(string(stdout))
			return
		}
	}
}

func runOutputLoop(o chan []byte) {
	dt := &dnstap.Dnstap{}
	for frame := range o {
		if err := proto.Unmarshal(frame, dt); err != nil {
			panic(err)
		}
		switch *dt.Message.Type {
		case dnstap.Message_CLIENT_RESPONSE:
			{
				msg := new(dns.Msg)
				err := msg.Unpack(dt.Message.ResponseMessage)
				if err != nil {
					panic(err)
				}
				for _, i := range msg.Question {
					for _, j := range msg.Answer {
						if t, ok := j.(*dns.A); ok {
							// here we have answers to A records
							fmt.Printf("%s -> %s\n", i.Name, t.A.String())
							allowTrafficIfNeeded(i.Name, t.A.String())
						}
						// fmt.Printf("response [%d] %s -> (%s) %d %s\n", j.Header().Rrtype, i.Name, j.String(), j.Header().Rrtype, j.Header().Name)
						// should check the table name -> podIP
					}
				}

			}
		}

	}
}
func runInput(i dnstap.Input, o chan []byte, wg *sync.WaitGroup) {
	go i.ReadInto(o)
	i.Wait()
	wg.Done()
}

func main() {

	kubeconfig := flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	node := flag.String("node", "", "node where this thing is running on")
	tapsock := flag.String("tapsock", "/var/run/dns/dnstap.sock", "path to the dnstap unix sock")
	secon := flag.String("secon", "", "selinux context for the dnstap socket file")
	flag.Parse()

	var config *rest.Config
	var err error
	if *kubeconfig != "" {
		// use the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			panic(err.Error())
		}
	} else {
		fmt.Println("defaulting to in cluster config")
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	var iwg sync.WaitGroup
	dnstapinput, err := dnstap.NewFrameStreamSockInputFromPath(*tapsock)
	if err != nil {
		panic(err)
	}

	// we add some hack to let CoreDNS write stuff on our socket
	if *secon != "" {
		if err := selinux.Chcon(*tapsock, *secon, false); err != nil {
			panic(err)
		}
	}

	c := make(chan []byte, 102400)
	go runOutputLoop(c)
	go runInput(dnstapinput, c, &iwg)

	for {
		// get the pods
		pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", *node),
		})
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("There are %d pods in node %s\n", len(pods.Items), *node)

		namespaceToIPs := make(map[string][]string)
		for _, pod := range pods.Items {
			if pod.Spec.HostNetwork {
				fmt.Printf("skipping pod with hostNetwork\n")
				continue
			}
			if strings.Count(pod.Status.PodIP, ".") != 3 {
				continue
			}
			fmt.Printf("namespace %s has IP %s\n", pod.Namespace, pod.Status.PodIP)
			if val, ok := namespaceToIPs[pod.Namespace]; ok {
				namespaceToIPs[pod.Namespace] = append(val, pod.Status.PodIP)
			} else {
				namespaceToIPs[pod.Namespace] = []string{pod.Status.PodIP}
			}
		}

		nameToSourceIP := make(map[string][]string)
		// get the egressFirewall DNS rules
		egressFirewallClientSet := egressfirewallclientset.NewForConfigOrDie(config)
		efws, err := egressFirewallClientSet.K8sV1().EgressFirewalls("").List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			panic(err.Error())
		}
		for _, efw := range efws.Items {
			ips := namespaceToIPs[efw.Namespace]
			if len(ips) == 0 {
				continue
			}

			fmt.Printf("in namespace %s\n", efw.Namespace)
			for _, i := range efw.Spec.Egress {
				if i.Type != v1.EgressFirewallRuleAllow || len(i.To.DNSName) < 4 {
					continue
				}

				fmt.Printf("  %s\n", i.To.DNSName)

				if val, ok := nameToSourceIP[i.To.DNSName]; ok {
					nameToSourceIP[i.To.DNSName] = append(val, ips...)
				} else {
					nameToSourceIP[i.To.DNSName] = ips
				}
			}

		}
		mu.Lock()
		gnameToSourceIP = nameToSourceIP
		nameToSourceIP = nil
		mu.Unlock()

		time.Sleep(10 * time.Second)
	}
}
