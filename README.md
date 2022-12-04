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



# Running

## in localdev

we start ovn-kubernetes kind environment, and retrieve its config.
we start a local Coredns process, here's an example of Corefile to get started:
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

In ovn-kubernetes localdev, CoreDNS is targetted as a service that is not necessarily running on the same node as our pod. We make sure we have a single instance of CoreDNS so that everythign runs on the same node:
```
k scale deployment -n kube-system coredns --replicas=1
```


We will be able to experiment on the node where coredns is running:

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


