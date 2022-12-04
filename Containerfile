FROM localhost/ovn-daemonset-f:dev
# this one is created on kind, and contains ovs-ofctl already
COPY dnsflow /dnsflow
ENTRYPOINT [ "/dnsflow" ]