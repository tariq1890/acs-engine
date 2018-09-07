package kubernetesupgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/Azure/acs-engine/pkg/acsengine"
	"github.com/Azure/acs-engine/pkg/acsengine/transform"
	"github.com/Azure/acs-engine/pkg/api"
	"github.com/Azure/acs-engine/pkg/armhelpers"
	"github.com/Azure/acs-engine/pkg/armhelpers/utils"
	"github.com/Azure/acs-engine/pkg/i18n"
	"github.com/Azure/acs-engine/pkg/operations"
	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
)

// Upgrader holds information on upgrading an ACS cluster
type Upgrader struct {
	Translator *i18n.Translator
	logger     *logrus.Entry
	ClusterTopology
	Client           armhelpers.ACSEngineClient
	kubeConfig       string
	stepTimeout      *time.Duration
	ACSEngineVersion string
}

type vmStatus int

const (
	defaultTimeout            = time.Minute * 10
	vmStatusUpgraded vmStatus = iota
	vmStatusNotUpgraded
)

type vmInfo struct {
	name   string
	status vmStatus
}

// Init initializes an upgrader struct
func (ku *Upgrader) Init(translator *i18n.Translator, logger *logrus.Entry, clusterTopology ClusterTopology, client armhelpers.ACSEngineClient, kubeConfig string, stepTimeout *time.Duration, acsEngineVersion string) {
	ku.Translator = translator
	ku.logger = logger
	ku.ClusterTopology = clusterTopology
	ku.Client = client
	ku.kubeConfig = kubeConfig
	ku.stepTimeout = stepTimeout
	ku.ACSEngineVersion = acsEngineVersion
}

// RunUpgrade runs the upgrade pipeline
func (ku *Upgrader) RunUpgrade() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	if err := ku.upgradeMasterNodes(ctx); err != nil {
		return err
	}

	if err := ku.upgradeAgentScaleSets(ctx); err != nil {
		return err
	}

	return ku.upgradeAgentPools(ctx)
}

// Validate will run validation post upgrade
func (ku *Upgrader) Validate() error {
	return nil
}

func (ku *Upgrader) upgradeMasterNodes(ctx context.Context) error {
	if ku.ClusterTopology.DataModel.Properties.MasterProfile == nil {
		return nil
	}
	ku.logger.Infof("Master nodes StorageProfile: %s", ku.ClusterTopology.DataModel.Properties.MasterProfile.StorageProfile)
	// Upgrade Master VMs
	templateMap, parametersMap, err := ku.generateUpgradeTemplate(ku.ClusterTopology.DataModel, ku.ACSEngineVersion)
	if err != nil {
		return ku.Translator.Errorf("error generating upgrade template: %s", err.Error())
	}

	ku.logger.Infof("Prepping master nodes for upgrade...")

	transformer := &transform.Transformer{
		Translator: ku.Translator,
	}
	if err := transformer.NormalizeResourcesForK8sMasterUpgrade(ku.logger, templateMap, ku.DataModel.Properties.MasterProfile.IsManagedDisks(), nil); err != nil {
		ku.logger.Errorf(err.Error())
		return err
	}

	upgradeMasterNode := UpgradeMasterNode{
		Translator: ku.Translator,
		logger:     ku.logger,
	}
	upgradeMasterNode.TemplateMap = templateMap
	upgradeMasterNode.ParametersMap = parametersMap
	upgradeMasterNode.UpgradeContainerService = ku.ClusterTopology.DataModel
	upgradeMasterNode.ResourceGroup = ku.ClusterTopology.ResourceGroup
	upgradeMasterNode.SubscriptionID = ku.ClusterTopology.SubscriptionID
	upgradeMasterNode.Client = ku.Client
	upgradeMasterNode.kubeConfig = ku.kubeConfig
	if ku.stepTimeout == nil {
		upgradeMasterNode.timeout = defaultTimeout
	} else {
		upgradeMasterNode.timeout = *ku.stepTimeout
	}

	mastersToUpgradeCount := ku.ClusterTopology.DataModel.Properties.MasterProfile.Count

	ku.logger.Infof("Master nodes that need to be upgraded: %d", mastersToUpgradeCount)

	ku.logger.Infof("Starting upgrade of master nodes...")

	masterNodesInCluster := len(*ku.ClusterTopology.MasterVMs)
	ku.logger.Infof("masterNodesInCluster: %d", masterNodesInCluster)
	if masterNodesInCluster > mastersToUpgradeCount {
		return ku.Translator.Errorf("Total count of master VMs: %d exceeded expected count: %d", masterNodesInCluster, mastersToUpgradeCount)
	}

	upgradedMastersIndex := make(map[int]bool)

	for _, vm := range *ku.ClusterTopology.MasterVMs {
		ku.logger.Infof("Upgrading Master VM: %s", *vm.Name)

		masterIndex, _ := utils.GetVMNameIndex(vm.StorageProfile.OsDisk.OsType, *vm.Name)

		err := upgradeMasterNode.DeleteNode(vm.Name, false)
		if err != nil {
			ku.logger.Infof("Error deleting master VM: %s, err: %v", *vm.Name, err)
			return err
		}

		err = upgradeMasterNode.CreateNode(ctx, "master", masterIndex)
		if err != nil {
			ku.logger.Infof("Error creating upgraded master VM: %s", *vm.Name)
			return err
		}

		err = upgradeMasterNode.Validate(vm.Name)
		if err != nil {
			ku.logger.Infof("Error validating upgraded master VM: %s", *vm.Name)
			return err
		}

		upgradedMastersIndex[masterIndex] = true
	}

	// This condition is possible if the previous upgrade operation failed during master
	// VM upgrade when a master VM was deleted but creation of upgraded master did not run.
	if masterNodesInCluster < mastersToUpgradeCount {
		ku.logger.Infof(
			"Found missing master VMs in the cluster. Reconstructing names of missing master VMs for recreation during upgrade...")
	}

	mastersToCreate := mastersToUpgradeCount - masterNodesInCluster
	ku.logger.Infof("Expected master count: %d, Creating %d more master VMs", mastersToUpgradeCount, mastersToCreate)

	// NOTE: this is NOT completely idempotent because it assumes that
	// the OS disk has been deleted
	for i := 0; i < mastersToCreate; i++ {
		masterIndexToCreate := 0
		for upgradedMastersIndex[masterIndexToCreate] {
			masterIndexToCreate++
		}

		ku.logger.Infof("Creating upgraded master VM with index: %d", masterIndexToCreate)

		err = upgradeMasterNode.CreateNode(ctx, "master", masterIndexToCreate)
		if err != nil {
			ku.logger.Infof("Error creating upgraded master VM with index: %d", masterIndexToCreate)
			return err
		}

		tempVMName := ""
		err = upgradeMasterNode.Validate(&tempVMName)
		if err != nil {
			ku.logger.Infof("Error validating upgraded master VM with index: %d", masterIndexToCreate)
			return err
		}

		upgradedMastersIndex[masterIndexToCreate] = true
	}

	return nil
}

func (ku *Upgrader) upgradeAgentPools(ctx context.Context) error {
	for _, agentPool := range ku.ClusterTopology.AgentPools {
		// Upgrade Agent VMs
		templateMap, parametersMap, err := ku.generateUpgradeTemplate(ku.ClusterTopology.DataModel, ku.ACSEngineVersion)
		if err != nil {
			ku.logger.Errorf("Error generating upgrade template: %v", err)
			return ku.Translator.Errorf("Error generating upgrade template: %s", err.Error())
		}

		ku.logger.Infof("Prepping agent pool '%s' for upgrade...", *agentPool.Name)

		preservePools := map[string]bool{*agentPool.Name: true}
		transformer := &transform.Transformer{
			Translator: ku.Translator,
		}
		var isMasterManagedDisk bool
		if ku.DataModel.Properties.MasterProfile != nil {
			isMasterManagedDisk = ku.DataModel.Properties.MasterProfile.IsManagedDisks()
		}
		if err := transformer.NormalizeResourcesForK8sAgentUpgrade(ku.logger, templateMap, isMasterManagedDisk, preservePools); err != nil {
			ku.logger.Errorf(err.Error())
			return ku.Translator.Errorf("Error generating upgrade template: %s", err.Error())
		}

		var agentCount, agentPoolIndex int
		var agentOsType api.OSType
		var agentPoolName string
		for indx, app := range ku.ClusterTopology.DataModel.Properties.AgentPoolProfiles {
			if app.Name == *agentPool.Name {
				agentCount = app.Count
				agentOsType = app.OSType
				agentPoolName = app.Name
				agentPoolIndex = indx
				break
			}
		}

		if agentCount == 0 {
			ku.logger.Infof("Agent pool '%s' is empty", *agentPool.Name)
			return nil
		}

		upgradeAgentNode := UpgradeAgentNode{
			Translator: ku.Translator,
			logger:     ku.logger,
		}
		upgradeAgentNode.TemplateMap = templateMap
		upgradeAgentNode.ParametersMap = parametersMap
		upgradeAgentNode.UpgradeContainerService = ku.ClusterTopology.DataModel
		upgradeAgentNode.SubscriptionID = ku.ClusterTopology.SubscriptionID
		upgradeAgentNode.ResourceGroup = ku.ClusterTopology.ResourceGroup
		upgradeAgentNode.Client = ku.Client
		upgradeAgentNode.kubeConfig = ku.kubeConfig
		if ku.stepTimeout == nil {
			upgradeAgentNode.timeout = defaultTimeout
		} else {
			upgradeAgentNode.timeout = *ku.stepTimeout
		}

		agentVMs := make(map[int]*vmInfo)
		for _, vm := range *agentPool.AgentVMs {
			agentIndex, _ := utils.GetVMNameIndex(vm.StorageProfile.OsDisk.OsType, *vm.Name)
			agentVMs[agentIndex] = &vmInfo{*vm.Name, vmStatusNotUpgraded}
		}
		toBeUpgradedCount := len(*agentPool.AgentVMs)

		ku.logger.Infof("Starting upgrade of %d agent nodes (out of %d) in pool identifier: %s, name: %s...",
			toBeUpgradedCount, agentCount, *agentPool.Identifier, *agentPool.Name)

		// Create missing nodes to match agentCount. This could be due to previous upgrade failure
		// If there are nodes that need to be upgraded, create one extra node, which will be used to take on the load from upgrading nodes.
		if toBeUpgradedCount > 0 {
			agentCount++
		} else {
			ku.logger.Infof("No nodes to upgrade")
			return nil
		}

		i := toBeUpgradedCount

		for i < agentCount {
			agentIndex := getAvailableIndex(agentVMs)

			vmName, err := utils.GetK8sVMName(agentOsType, ku.DataModel.Properties.HostedMasterProfile != nil,
				ku.NameSuffix, agentPoolName, agentPoolIndex, agentIndex)
			if err != nil {
				ku.logger.Errorf("Error reconstructing agent VM name with index %d: %v", agentIndex, err)
				return err
			}
			ku.logger.Infof("Creating new agent node %s (index %d)", vmName, agentIndex)

			err = upgradeAgentNode.CreateNode(ctx, *agentPool.Name, agentIndex)
			if err != nil {
				ku.logger.Errorf("Error creating agent node %s (index %d): %v", vmName, agentIndex, err)
				return err
			}

			err = upgradeAgentNode.Validate(&vmName)
			if err != nil {
				ku.logger.Infof("Error validating agent node %s (index %d): %v", vmName, agentIndex, err)
				return err
			}

			agentVMs[agentIndex] = &vmInfo{vmName, vmStatusUpgraded}

			i++
		}

		// Upgrade nodes in agent pool
		upgradedCount := 0
		for agentIndex, vm := range agentVMs {
			if vm.status != vmStatusNotUpgraded {
				continue
			}
			ku.logger.Infof("Upgrading Agent VM: %s, pool name: %s", vm.name, *agentPool.Name)

			err := upgradeAgentNode.DeleteNode(&vm.name, true)
			if err != nil {
				ku.logger.Errorf("Error deleting agent VM %s: %v", vm.name, err)
				return err
			}

			// do not create last node in favor of already created extra node.
			if upgradedCount == toBeUpgradedCount-1 {
				ku.logger.Infof("Skipping creation of VM %s (index %d)", vm.name, agentIndex)
				delete(agentVMs, agentIndex)
			} else {
				err = upgradeAgentNode.CreateNode(ctx, *agentPool.Name, agentIndex)
				if err != nil {
					ku.logger.Errorf("Error creating upgraded agent VM %s: %v", vm.name, err)
					return err
				}

				err = upgradeAgentNode.Validate(&vm.name)
				if err != nil {
					ku.logger.Errorf("Error validating upgraded agent VM %s: %v", vm.name, err)
					return err
				}
				vm.status = vmStatusUpgraded
			}
			upgradedCount++
		}
	}

	return nil
}

func (ku *Upgrader) upgradeAgentScaleSets(ctx context.Context) error {
	if len(ku.ClusterTopology.AgentPoolScaleSetsToUpgrade) > 0 {
		// need to apply the ARM template with target Kubernetes version to the VMSS first in order that the new VMSS instances
		// created can get the expected Kubernetes version. Otherwise the new instances created still have old Kubernetes version
		// if the topology doesn't have master nodes (so there are no ARM deployments in previous upgradeMasterNodes step)
		templateMap, parametersMap, err := ku.generateUpgradeTemplate(ku.ClusterTopology.DataModel, ku.ACSEngineVersion)
		if err != nil {
			ku.logger.Errorf("error generating upgrade template in upgradeAgentScaleSets: %v", err)
			return err
		}

		transformer := &transform.Transformer{
			Translator: ku.Translator,
		}

		if err := transformer.NormalizeForVMSSScaling(ku.logger, templateMap); err != nil {
			ku.logger.Errorf("unable to update template, error: %v.", err)
			return err
		}

		random := rand.New(rand.NewSource(time.Now().UnixNano()))
		deploymentSuffix := random.Int31()
		deploymentName := fmt.Sprintf("agentscaleset-%s-%d", time.Now().Format("06-01-02T15.04.05"), deploymentSuffix)

		ku.logger.Infof("Deploying the agent scale sets ARM template...")
		_, err = ku.Client.DeployTemplate(
			ctx,
			ku.ClusterTopology.ResourceGroup,
			deploymentName,
			templateMap,
			parametersMap)

		if err != nil {
			ku.logger.Errorf("error applying upgrade template in upgradeAgentScaleSets: %v", err)
			return err
		}
	}

	for _, vmssToUpgrade := range ku.ClusterTopology.AgentPoolScaleSetsToUpgrade {
		ku.logger.Infof("Upgrading VMSS %s", vmssToUpgrade.Name)

		if len(vmssToUpgrade.VMsToUpgrade) == 0 {
			ku.logger.Infof("No VMs to upgrade for VMSS %s, skipping", vmssToUpgrade.Name)
			continue
		}

		newCapacity := *vmssToUpgrade.Sku.Capacity + 1
		ku.logger.Infof(
			"VMSS %s current capacity is %d and new capacity will be %d while each node is swapped",
			vmssToUpgrade.Name,
			*vmssToUpgrade.Sku.Capacity,
			newCapacity,
		)

		*vmssToUpgrade.Sku.Capacity = newCapacity

		for _, vmToUpgrade := range vmssToUpgrade.VMsToUpgrade {
			if err := ku.Client.SetVirtualMachineScaleSetCapacity(
				ctx,
				ku.ClusterTopology.ResourceGroup,
				vmssToUpgrade.Name,
				vmssToUpgrade.Sku,
				vmssToUpgrade.Location,
			); err != nil {
				ku.logger.Errorf("Failure to set capacity for VMSS %s", vmssToUpgrade.Name)
				return err
			}

			ku.logger.Infof("Successfully set capacity for VMSS %s", vmssToUpgrade.Name)

			// Before we can delete the node we should safely and responsibly drain it
			var kubeAPIServerURL string
			getClientTimeout := 10 * time.Second

			if ku.DataModel.Properties.HostedMasterProfile != nil {
				kubeAPIServerURL = ku.DataModel.Properties.HostedMasterProfile.FQDN
			} else {
				kubeAPIServerURL = ku.DataModel.Properties.MasterProfile.FQDN
			}
			client, err := ku.Client.GetKubernetesClient(
				kubeAPIServerURL,
				ku.kubeConfig,
				interval,
				getClientTimeout,
			)
			if err != nil {
				ku.logger.Errorf("Error getting Kubernetes client: %v", err)
				return err
			}

			ku.logger.Infof("Draining node %s", vmToUpgrade.Name)
			err = operations.SafelyDrainNodeWithClient(
				client,
				ku.logger,
				vmToUpgrade.Name,
				time.Minute,
			)
			if err != nil {
				ku.logger.Errorf("Error draining VM in VMSS: %v", err)
				return err
			}

			ku.logger.Infof(
				"Deleting VM %s in VMSS %s",
				vmToUpgrade.Name,
				vmssToUpgrade.Name,
			)

			// At this point we have our buffer node that will replace the node to delete
			// so we can just remove this current node then
			if err := ku.Client.DeleteVirtualMachineScaleSetVM(
				ctx,
				ku.ClusterTopology.ResourceGroup,
				vmssToUpgrade.Name,
				vmToUpgrade.InstanceID,
			); err != nil {
				ku.logger.Errorf(
					"Failed to delete VM %s in VMSS %s",
					vmToUpgrade.Name,
					vmssToUpgrade)
				return err
			}

			ku.logger.Infof(
				"Successfully deleted VM %s in VMSS %s",
				vmToUpgrade.Name,
				vmssToUpgrade.Name)
		}
		ku.logger.Infof("Completed upgrading VMSS %s", vmssToUpgrade)
	}

	ku.logger.Infoln("Completed upgrading all VMSS")

	return nil
}

func (ku *Upgrader) generateUpgradeTemplate(upgradeContainerService *api.ContainerService, acsengineVersion string) (map[string]interface{}, map[string]interface{}, error) {
	var err error
	ctx := acsengine.Context{
		Translator: ku.Translator,
	}
	templateGenerator, err := acsengine.InitializeTemplateGenerator(ctx)
	if err != nil {
		return nil, nil, ku.Translator.Errorf("failed to initialize template generator: %s", err.Error())
	}

	var templateJSON string
	var parametersJSON string
	if templateJSON, parametersJSON, _, err = templateGenerator.GenerateTemplate(upgradeContainerService, acsengine.DefaultGeneratorCode, true, false, acsengineVersion); err != nil {
		return nil, nil, ku.Translator.Errorf("error generating upgrade template: %s", err.Error())
	}

	var template interface{}
	var parameters interface{}
	json.Unmarshal([]byte(templateJSON), &template)
	json.Unmarshal([]byte(parametersJSON), &parameters)
	templateMap := template.(map[string]interface{})
	parametersMap := parameters.(map[string]interface{})

	return templateMap, parametersMap, nil
}

// return unused index within the range of agent indices, or subsequent index
func getAvailableIndex(vms map[int]*vmInfo) int {
	maxIndex := 0

	for indx := range vms {
		if indx > maxIndex {
			maxIndex = indx
		}
	}

	for indx := 0; indx < maxIndex; indx++ {
		if _, found := vms[indx]; !found {
			return indx
		}
	}

	return maxIndex + 1
}

// isNodeReady returns true if a node is ready; false otherwise.
// Copied from: https://github.com/kubernetes/kubernetes/blob/886e04f1fffbb04faf8a9f9ee141143b2684ae68/pkg/api/v1/node/util.go#L40
func isNodeReady(node *v1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == v1.NodeReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}
