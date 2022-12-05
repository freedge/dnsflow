Support adding flows dynamically for OVN-Kubernetes egress firewall. This is a proof of concept.

- we use [dnstap](https://coredns.io/plugins/dnstap/) CoreDNS plug-in to mirror the DNS traffic to our own pod
- we use this to inject extra flows into ovs to allow that traffic. We call ovs-ofctl directly
- we assume the CoreDNS is running on the same node as the pod itself.


EgressFirewall resources are normally processed by ovnk masters, that resolve the DNS names and add address_set in OVN nbdb, allowing egress traffic towards specific IP,. This comes with some limitations:
- there is no way to support wildcard DNS entries
- there is no guarantee that the pod will resolve the DNS name to the same IP as the controller did.


Our goal is to support rules like these:

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressFirewall
metadata:
  name: default
  namespace: abc
spec:
  egress:
  - to:
      dnsName: "google.com"
    type: Allow
  - to:
      cidrSelector: 0.0.0.0/0
    type: Deny
```

and that "curl www.google.com" on a pod is working, despite google.com possibly not existing or having a different IP than www.google.com.

We make some assumptions:
- each pod will try to resolve the DNS name it connects to
- TCP retransmission makes it so we don't really care if the initial TCP SYN arrives before a flow is installed
- we don't expect that much DNS traffic on the node
- it's essentially best effort
- this is a proof of concept and we do not care about cleaning flows we added
- in our proof of concept, we rebuild a list of interesting pods and egressfirewall resources every 10 seconds,
so if the DNS query arrives too early it will fail for sure

There could be other ideas, like using a http proxy like Squid to achieve the same thing for http/https traffic.

# Running

## in localdev

we start ovn-kubernetes [kind](https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/kind.md) environment, and retrieve its config.
we start a local CoreDNS process, here's an example of Corefile to get started:
```
.:1053 {
    forward . 8.8.8.8:53
    log
    dnstap /tmp/dnstap.sock full
}
```
we then start our dnsflow:

```
./dnsflow -kubeconfig ./config -tapsock /tmp/dnstap.sock -node ovn-worker
```

## in ovn-kubernetes kind environment

We edit the configmap for DNS config:

```bash
k edit -n kube-system configmap/coredns
```
to add a dnstap line:
```
Corefile: |
    .:53 {
      ...
      dnstap /var/run/dns/dnstap.sock full
    }
```


and patch the coredns deployment:

```yaml
spec:
  template:
    spec:
      containers:
      - name: coredns
        volumeMounts:
        - mountPath: /var/run/dns
          name: run-dns
      volumes:
      - hostPath:
          path: /var/run/dns
          type: ""
        name: run-dns
```

```bash
k patch deployment -n kube-system coredns --patch-file coredns-patch.yaml
```

In ovn-kubernetes localdev, CoreDNS is targetted as a service that is not necessarily running on the same node as our pod. We make sure we have a single instance of CoreDNS so that everything runs on the same node:
```
k scale deployment -n kube-system coredns --replicas=1
```


We will be able to experiment on the node where CoreDNS is running:

```bash
$ k get -n kube-system pods -o wide -l k8s-app=kube-dns
NAME                       READY   STATUS    RESTARTS   AGE   IP           NODE         NOMINATED NODE   READINESS GATES
coredns-7ddb785f56-p49fh   1/1     Running   0          77m   10.244.0.4   ovn-worker   <none>           <none>
```


We need to load our image into Kind:
this involves building an image locally, then  exporting it with 
```podman save localhost/dnsflow:0.0.1 -o dnsflow.tar```
then load it with ```sudo kind load image-archive dnsflow.tar --name ovn```

See Containerfile, dnsflowdaemon.yaml, rolebinding.yaml

We can run a pod on a specific node
```
k create namespace abc
k apply -f efw.yaml
k run -n abc --overrides="{\"spec\": {\"nodeSelector\": { \"kubernetes.io/hostname\": \"ovn-worker\" } } }"  --restart=Never --rm -ti --image alpine  myclient  -- sh
```


## in OpenShift

```
oc patch dns.operator.openshift.io default --type merge --patch '{"spec":{"managementState":"Unmanaged"}}'
oc edit configmap -n openshift-dns dns-default
# container is now called dns instead of coredns:
oc patch daemonset -n openshift-dns dns-default --patch-file coredns-patch.yaml
# cluster-role is now called openshift-ovn-kubernetes-controller instead of ovn-kubernetes
oc apply -f rolebinding.yaml
# check the image tag:
oc apply -f dnsflowdaemon.yaml
```

There is some permission issue where the coredns container cannot access the dnstap socket.
CoreDNS runs using a specific selinuxcontext.
[Doc on hostpath for persistent storage](https://docs.openshift.com/container-platform/4.11/storage/persistent_storage/persistent-storage-hostpath.html) mentions the need to run in privileged mode.

Instead we can fix a specific selinux context for CoreDNS:
```
spec:
  template:
    spec:
      containers:
      - name: dns
        securityContext:
          seLinuxOptions:
            level: "s0:c900,c901"
```

so we can now run 
```
chcon system_u:object_r:container_file_t:s0:c900,c901 /var/run/dns/dnstap.sock
```
making sure the file is now accessible.

This can be done by specifying ```-secon system_u:object_r:container_file_t:s0:c900,c901```.

Results for now:
- when the pod starts, if it immediately sends a DNS query, it is "lost" from dnsflow, which is only checking for new pods every 10s. For this we should watch for resource updates.
- right after the DNS query, it takes ~2s (or, 2 lost pings) before the flow gets installed.

It should be due (in part) to CoreDNS dnstap plug-in [flush timeout](https://github.com/coredns/coredns/blob/9b94696b115d2d1394388e2b15c8ff05e5273cdf/plugin/dnstap/io.go#L16) and probably also caused a little bit by the time it takes to add the flow.
