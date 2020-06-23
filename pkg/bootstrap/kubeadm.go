package bootstrap

import (
	"bytes"
	"fmt"
	"github.com/avast/retry-go"
	"github.com/hashicorp/go-multierror"
	"github.com/innobead/kubefire/pkg/data"
	"github.com/innobead/kubefire/pkg/node"
	"github.com/innobead/kubefire/pkg/script"
	utilssh "github.com/innobead/kubefire/pkg/util/ssh"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"sync"
	"time"
)

type KubeadmBootstrapper struct {
	nodeManager node.Manager
}

func NewKubeadmBootstrapper(nodeManager node.Manager) *KubeadmBootstrapper {
	return &KubeadmBootstrapper{
		nodeManager: nodeManager,
	}
}

func (k *KubeadmBootstrapper) Deploy(cluster *data.Cluster) error {
	if err := k.nodeManager.WaitNodesRunning(cluster.Name, 5); err != nil {
		return errors.WithMessage(err, "some nodes are not running")
	}

	if err := k.init(cluster); err != nil {
		return err
	}

	firstMaster, err := k.nodeManager.GetNode(node.NodeName(cluster.Name, node.Master, 1))
	if err != nil {
		return err
	}

	joinCmd, err := k.bootstrap(firstMaster, len(cluster.Nodes) == 0)
	if err != nil {
		return err
	}

	nodes, err := k.nodeManager.ListNodes(cluster.Name)
	if err != nil {
		return err
	}

	for _, n := range nodes {
		if n.Name == firstMaster.Name {
			continue
		}

		if err := k.join(n, joinCmd); err != nil {
			return err
		}
	}

	return nil
}

func (k *KubeadmBootstrapper) init(cluster *data.Cluster) error {
	logrus.Infof("initializing cluster (%s)", cluster.Name)

	wgInitNodes := sync.WaitGroup{}
	wgInitNodes.Add(len(cluster.Nodes))

	chInitNodesErrors := make(chan error, len(cluster.Nodes))

	for _, n := range cluster.Nodes {
		logrus.Infof("initializing node (%s)", n.Name)

		go func(n *data.Node) {
			defer wgInitNodes.Done()

			_ = retry.Do(func() error {
				sshClient, err := utilssh.NewClient(
					cluster.Spec.Prikey,
					"root",
					n.Status.IPAddresses,
					nil,
				)
				if err != nil {
					return err
				}
				defer sshClient.Close()

				cmds := []string{
					"zypper in -y tar curl ethtool socat ebtables iptables conntrack-tools", // FIXME workaround until figuring out how to use the latest image on ignite
					fmt.Sprintf("curl -sSLO %s", script.RemoteScriptUrl(script.InstallPrerequisitesKubeadm)),
					fmt.Sprintf("chmod +x %s", script.InstallPrerequisitesKubeadm),
					fmt.Sprintf("./%s", script.InstallPrerequisitesKubeadm),
					"sysctl -w net.ipv4.ip_forward=1",
					`echo "net.ipv4.ip_forward = 1" >> /etc/sysctl.conf`,
					`echo "0.0.0.0 $(hostname)" >> /etc/hosts`,
					`echo "export CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock" >> /etc/profile.d/containerd.sh`,
					"kubeadm init phase preflight -v 5",
				}

				err = sshClient.Run(nil, nil, cmds...)
				if err != nil {
					chInitNodesErrors <- errors.WithMessagef(err, "failed on node (%s)", n.Name)
				}

				return nil
			},
				retry.Delay(10*time.Second),
				retry.MaxDelay(1*time.Minute),
			)
		}(n)
	}

	logrus.Info("waiting all nodes initialization finished")

	wgInitNodes.Wait()
	close(chInitNodesErrors)

	var err error
	for {
		e, ok := <-chInitNodesErrors
		if !ok {
			break
		}

		if e != nil {
			err = multierror.Append(err, e)
		}
	}

	return err
}

func (k *KubeadmBootstrapper) bootstrap(node *data.Node, isSingleNode bool) (joinCmd string, err error) {
	logrus.Infof("bootstrapping the first master node (%s)", node.Name)

	sshClient, err := utilssh.NewClient(
		node.Spec.Cluster.Prikey,
		"root",
		node.Status.IPAddresses,
		nil,
	)
	if err != nil {
		return "", err
	}
	defer sshClient.Close()

	joinCmdBuf := bytes.Buffer{}

	cmds := []struct {
		cmdline string
		before  utilssh.Callback
		after   utilssh.Callback
	}{
		{
			cmdline: "kubeadm init -v 5",
			before: func(session *ssh.Session) bool {
				logrus.Info("running kubeadm init")
				return true
			},
		},
		{
			cmdline: "kubeadm token create --print-join-command",
			before: func(session *ssh.Session) bool {
				logrus.Info("creating the join command")
				session.Stdout = &joinCmdBuf
				return true
			},
		},
		{
			cmdline: "KUBECONFIG=/etc/kubernetes/admin.conf kubectl create -f https://raw.githubusercontent.com/cilium/cilium/v1.8/install/kubernetes/quick-install.yaml",
			before: func(session *ssh.Session) bool {
				logrus.Info("applying CNI network")
				return true
			},
		},
		{
			cmdline: "KUBECONFIG=/etc/kubernetes/admin.conf kubectl taint nodes --all node-role.kubernetes.io/master-",
			before: func(session *ssh.Session) bool {
				if isSingleNode {
					logrus.Infof("untainting the master node (%s)", node.Name)
				}

				return isSingleNode
			},
		},
	}

	for _, cmd := range cmds {
		err := sshClient.Run(cmd.before, cmd.after, cmd.cmdline)
		if err != nil {
			return "", errors.WithStack(err)
		}
	}

	return joinCmdBuf.String(), nil
}

func (k *KubeadmBootstrapper) join(node *data.Node, joinCmd string) error {
	logrus.Infof("joining node (%s)", node.Name)

	sshClient, err := utilssh.NewClient(
		node.Spec.Cluster.Prikey,
		"root",
		node.Status.IPAddresses,
		nil,
	)
	if err != nil {
		return err
	}
	defer sshClient.Close()

	logrus.Infof("running join command (%s)", joinCmd)

	if err := sshClient.Run(nil, nil, fmt.Sprintf("%s -v 5", joinCmd)); err != nil {
		return errors.WithStack(err)
	}

	return nil
}
