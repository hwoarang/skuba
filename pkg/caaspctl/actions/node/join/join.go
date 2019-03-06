package node

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	bootstraputil "k8s.io/cluster-bootstrap/token/util"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmscheme "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/scheme"
	"k8s.io/kubernetes/cmd/kubeadm/app/constants"
	kubeadmtokenphase "k8s.io/kubernetes/cmd/kubeadm/app/phases/bootstraptoken/node"
	kubeadmutil "k8s.io/kubernetes/cmd/kubeadm/app/util"
	kubeadmconfigutil "k8s.io/kubernetes/cmd/kubeadm/app/util/config"
	"k8s.io/kubernetes/cmd/kubeadm/app/util/config/strict"

	"suse.com/caaspctl/internal/pkg/caaspctl/deployments"
	"suse.com/caaspctl/internal/pkg/caaspctl/kubernetes"
	"suse.com/caaspctl/pkg/caaspctl"
)

func Join(joinConfiguration deployments.JoinConfiguration, target *deployments.Target) {
	statesToApply := []string{"kubelet.configure", "kubelet.enable", "kubeadm.join"}

	if joinConfiguration.Role == deployments.MasterRole {
		statesToApply = append([]string{"kubernetes.upload-secrets"}, statesToApply...)
	}

	target.Apply(joinConfiguration, statesToApply...)
}

func ConfigPath(role deployments.Role, target *deployments.Target) string {
	configPath := caaspctl.MachineConfFile(target.Target)
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		configPath = caaspctl.TemplatePathForRole(role)
	}

	joinConfiguration, err := joinConfigFileAndDefaultsToInternalConfig(configPath)
	if err != nil {
		log.Fatalf("error parsing configuration: %v", err)
	}
	addFreshTokenToJoinConfiguration(target.Target, joinConfiguration)
	addTargetInformationToJoinConfiguration(target, role, joinConfiguration)
	finalJoinConfigurationContents, err := kubeadmconfigutil.MarshalKubeadmConfigObject(joinConfiguration)
	if err != nil {
		log.Fatal("could not marshal configuration")
	}

	if err := ioutil.WriteFile(caaspctl.MachineConfFile(target.Target), finalJoinConfigurationContents, 0600); err != nil {
		log.Fatal("error writing specific machine configuration")
	}

	return caaspctl.MachineConfFile(target.Target)
}

func addFreshTokenToJoinConfiguration(target string, joinConfiguration *kubeadmapi.JoinConfiguration) {
	if joinConfiguration.Discovery.BootstrapToken == nil {
		joinConfiguration.Discovery.BootstrapToken = &kubeadmapi.BootstrapTokenDiscovery{}
	}
	joinConfiguration.Discovery.BootstrapToken.Token = createBootstrapToken(target)
	joinConfiguration.Discovery.TLSBootstrapToken = ""
}

func addTargetInformationToJoinConfiguration(target *deployments.Target, role deployments.Role, joinConfiguration *kubeadmapi.JoinConfiguration) {
	if ip := net.ParseIP(target.Target); ip != nil {
		if joinConfiguration.NodeRegistration.KubeletExtraArgs == nil {
			joinConfiguration.NodeRegistration.KubeletExtraArgs = map[string]string{}
		}
		joinConfiguration.NodeRegistration.KubeletExtraArgs["node-ip"] = target.Target
	}
	joinConfiguration.NodeRegistration.Name = target.Nodename
	joinConfiguration.NodeRegistration.KubeletExtraArgs["hostname-override"] = target.Nodename
	osRelease, _ := target.OSRelease()
	if strings.Contains(osRelease["ID_LIKE"], "suse") {
		joinConfiguration.NodeRegistration.KubeletExtraArgs["cni-bin-dir"] = "/usr/lib/cni"
	}
}

func createBootstrapToken(target string) string {
	client := kubernetes.GetAdminClientSet()

	internalCfg, err := kubeadmconfigutil.ConfigFileAndDefaultsToInternalConfig(caaspctl.KubeadmInitConfFile(), nil)
	if err != nil {
		log.Fatal("could not load init configuration")
	}

	bootstrapTokenRaw, err := bootstraputil.GenerateBootstrapToken()
	if err != nil {
		log.Fatal("could not generate a new boostrap token")
	}

	bootstrapToken, err := kubeadmapi.NewBootstrapTokenString(bootstrapTokenRaw)
	if err != nil {
		log.Fatal("could not generate a new boostrap token")
	}

	internalCfg.BootstrapTokens = []kubeadmapi.BootstrapToken{
		kubeadmapi.BootstrapToken{
			Token:       bootstrapToken,
			Description: fmt.Sprintf("Bootstrap token for %s machine join", target),
			TTL: &metav1.Duration{
				Duration: 15 * time.Minute,
			},
			Usages: []string{"signing", "authentication"},
			Groups: []string{"system:bootstrappers:kubeadm:default-node-token"},
		},
	}

	if err := kubeadmtokenphase.CreateNewTokens(client, internalCfg.BootstrapTokens); err != nil {
		log.Fatal("could not create new bootstrap token")
	}

	return bootstrapTokenRaw
}

func joinConfigFileAndDefaultsToInternalConfig(cfgPath string) (*kubeadmapi.JoinConfiguration, error) {
	internalcfg := &kubeadmapi.JoinConfiguration{}

	b, err := ioutil.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}

	if err := kubeadmconfigutil.DetectUnsupportedVersion(b); err != nil {
		return nil, err
	}

	gvkmap, err := kubeadmutil.SplitYAMLDocuments(b)
	if err != nil {
		return nil, err
	}

	joinBytes := []byte{}
	for gvk, bytes := range gvkmap {
		if gvk.Kind == constants.JoinConfigurationKind {
			joinBytes = bytes
			// verify the validity of the YAML
			strict.VerifyUnmarshalStrict(bytes, gvk)
		}
	}

	if len(joinBytes) == 0 {
		return nil, errors.New("invalid config")
	}

	if err := runtime.DecodeInto(kubeadmscheme.Codecs.UniversalDecoder(), joinBytes, internalcfg); err != nil {
		return nil, err
	}

	return internalcfg, nil
}