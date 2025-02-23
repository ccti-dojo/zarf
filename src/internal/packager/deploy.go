package packager

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/defenseunicorns/zarf/src/types"

	"github.com/defenseunicorns/zarf/src/config"
	"github.com/defenseunicorns/zarf/src/internal/git"
	"github.com/defenseunicorns/zarf/src/internal/helm"
	"github.com/defenseunicorns/zarf/src/internal/images"
	"github.com/defenseunicorns/zarf/src/internal/k8s"
	"github.com/defenseunicorns/zarf/src/internal/message"
	"github.com/defenseunicorns/zarf/src/internal/template"
	"github.com/defenseunicorns/zarf/src/internal/utils"
	"github.com/mholt/archiver/v3"
	"github.com/otiai10/copy"
	"github.com/pterm/pterm"
	corev1 "k8s.io/api/core/v1"
)

var valueTemplate template.Values
var connectStrings = make(types.ConnectStrings)

// Deploy attempts to deploy a Zarf package that is define within the global DeployOptions struct
func Deploy() {
	message.Debug("packager.Deploy()")

	tempPath := createPaths()
	defer tempPath.clean()

	spinner := message.NewProgressSpinner("Preparing zarf package %s", config.DeployOptions.PackagePath)
	defer spinner.Stop()

	// Make sure the user gave us a package we can work with
	if utils.InvalidPath(config.DeployOptions.PackagePath) {
		spinner.Fatalf(nil, "Unable to find the package on the local system, expected package at %s", config.DeployOptions.PackagePath)
	}

	// Extract the archive
	spinner.Updatef("Extracting the package, this may take a few moments")
	err := archiver.Unarchive(config.DeployOptions.PackagePath, tempPath.base)
	if err != nil {
		spinner.Fatalf(err, "Unable to extract the package contents")
	}

	// Load the config from the extracted archive zarf.yaml
	spinner.Updatef("Loading the zarf package config")
	configPath := filepath.Join(tempPath.base, "zarf.yaml")
	if err := config.LoadConfig(configPath, true); err != nil {
		spinner.Fatalf(err, "Invalid or unreadable zarf.yaml file in %s", tempPath.base)
	}

	if config.IsZarfInitConfig() {
		// If init config, make sure things are ready
		utils.RunPreflightChecks()
	}

	spinner.Success()

	// If SBOM files exist, temporary place them in the deploy directory
	sbomViewFiles, _ := filepath.Glob(filepath.Join(tempPath.sboms, "sbom-viewer-*"))
	err = writeSBOMFiles(sbomViewFiles)
	if err != nil {
		message.Errorf(err, "Unable to process the SBOM files for this package")
		// Don't stop the deployment, let the user decide if they want to continue the deployment
	}

	// Confirm the overall package deployment
	confirm := confirmAction("Deploy", sbomViewFiles)

	// Don't continue unless the user says so
	if !confirm {
		return
	}

	// Generate a secret that describes the package that is being deployed
	secretName := fmt.Sprintf("zarf-package-%s", config.GetActiveConfig().Metadata.Name)
	deployedPackageSecret := k8s.GenerateSecret("zarf", secretName, corev1.SecretTypeOpaque)
	deployedPackageSecret.Labels["package-deploy-info"] = config.GetActiveConfig().Metadata.Name
	deployedPackageSecret.StringData = make(map[string]string)

	installedZarfPackage := types.DeployedPackage{
		Name:               config.GetActiveConfig().Metadata.Name,
		CLIVersion:         config.CLIVersion,
		Data:               config.GetActiveConfig(),
		DeployedComponents: make([]types.DeployedComponent, 0),
	}

	// Set variables and prompt if --confirm is not set
	if err := config.SetActiveVariables(); err != nil {
		message.Fatalf(err, "Unable to set variables in config: %s", err.Error())
	}

	// Verify the components requested all exist
	components := config.GetComponents()
	componentOptions := config.DeployOptions.Components

	// Init packages use a different component list
	if config.IsZarfInitConfig() {
		componentOptions = config.InitOptions.Components
	}

	// The component list is comma-delimited list
	var requestedComponents []string
	if componentOptions != "" {
		requestedComponents = strings.Split(componentOptions, ",")
	}

	// Get a list of all the components we are deploying and actually deploy them
	componentsToDeploy := getValidComponents(components, requestedComponents)
	deployedComponents, err := deployComponents(tempPath, componentsToDeploy)
	if err != nil {
		message.Errorf(err, "Unable to deploy all the components of this Zarf Package.")
	}
	installedZarfPackage.DeployedComponents = deployedComponents

	// Notify all the things about the successful deployment
	message.SuccessF("Zarf deployment complete")
	pterm.Println()
	printTablesForDeployment(componentsToDeploy)

	// Save deployed package information to k8s
	// Note: Not all packages need k8s; check if k8s is being used before saving the secret
	if packageUsesK8s() {
		stateData, _ := json.Marshal(installedZarfPackage)
		deployedPackageSecret.Data = map[string][]byte{"data": stateData}
		k8s.ReplaceSecret(deployedPackageSecret)
	}
}

// deployComponents loops through a list of ZarfComponents and deploys them
func deployComponents(tempPath tempPaths, componentsToDeploy []types.ZarfComponent) ([]types.DeployedComponent, error) {
	// When pushing images, the default behavior is to add a shasum of the url to the image name
	deployedComponents := []types.DeployedComponent{}
	config.SetDeployingComponents(deployedComponents)
	// Deploy all the components
	for _, component := range componentsToDeploy {
		deployedComponent := types.DeployedComponent{Name: component.Name}
		addShasumToImg := true

		// If this is an init-package and we are using an external registry, don't deploy the components to stand up an internal registry
		// TODO: Figure out a better way to do this (I don't like how these components are still `required` according to the yaml definition)
		if (config.IsZarfInitConfig() && config.InitOptions.RegistryInfo.Address != "") &&
			(component.Name == "zarf-seed-registry" || component.Name == "zarf-injector" || component.Name == "zarf-registry") {
			message.Notef("Not deploying the component (%s) since external registry information was provided during `zarf init`", component.Name)
			continue
		}

		// Do somewhat custom pre-configuration for the seed and agent components
		if config.IsZarfInitConfig() && component.Name == "zarf-seed-registry" && config.InitOptions.RegistryInfo.Address == "" {
			// The zarf-seed-registry component is responsible for seeding the state and finding a pod to inject a registry into
			seedZarfState(tempPath)
			runInjectionMadness(tempPath)
		} else if config.IsZarfInitConfig() && component.Name == "zarf-agent" {
			// The zarf-agent cannot mutate itself, so don't change the img url
			addShasumToImg = false

			// If we are using an external registry, we will need to seed the ZarfState as part of the zarf-agent component
			if !config.GetContainerRegistryInfo().InternalRegistry {
				seedZarfState(tempPath)
			}
		}

		// Actually deploy the component
		installedCharts := deployComponent(tempPath, component, addShasumToImg)

		// Do cleanup for when we inject the seed registry during initialization
		if config.IsZarfInitConfig() && component.Name == "zarf-seed-registry" {
			err := postSeedRegistry(tempPath)
			if err != nil {
				message.Warnf("Unable to seed the Zarf registry")
				return deployedComponents, fmt.Errorf("unable to seed the Zarf Registry: %w", err)
			}
		}

		// Deploy the component
		deployedComponent.InstalledCharts = installedCharts
		deployedComponents = append(deployedComponents, deployedComponent)
		config.SetDeployingComponents(deployedComponents)
	}
	config.ClearDeployingComponents()
	return deployedComponents, nil
}

// Deploy a Zarf Component
func deployComponent(tempPath tempPaths, component types.ZarfComponent, addShasumToImgs bool) []types.InstalledChart {
	var installedCharts []types.InstalledChart
	message.Debugf("packager.deployComponent(%#v, %#v", tempPath, component)

	// Toggles for general deploy operations
	componentPath := createComponentPaths(tempPath.components, component)

	// All components now require a name
	message.HeaderInfof("📦 %s COMPONENT", strings.ToUpper(component.Name))

	hasImages := len(component.Images) > 0
	hasCharts := len(component.Charts) > 0
	hasManifests := len(component.Manifests) > 0
	hasRepos := len(component.Repos) > 0
	hasDataInjections := len(component.DataInjections) > 0

	// Run the 'before' scripts and move files before we do anything else
	runComponentScripts(component.Scripts.Before, component.Scripts)
	processComponentFiles(component.Files, componentPath.files, tempPath.base)

	// Generate a value template
	valueTemplate = template.Generate()
	if !valueTemplate.Ready() && (hasImages || hasCharts || hasManifests || hasRepos) {
		valueTemplate = getUpdatedValueTemplate(component)
	}

	/* Install all the parts of the component */
	if hasImages {
		pushImagesToRegistry(tempPath, component.Images, addShasumToImgs)
	}

	if hasRepos {
		pushReposToRepository(componentPath.repos, component.Repos)
	}

	if hasDataInjections {
		waitGroup := sync.WaitGroup{}
		defer waitGroup.Wait()
		performDataInjections(&waitGroup, componentPath, component.DataInjections)
	}

	if hasCharts || hasManifests {
		installedCharts = installChartAndManifests(componentPath, component)
	}

	// Run the 'after' scripts after all other attributes of the component has been deployed
	runComponentScripts(component.Scripts.After, component.Scripts)

	return installedCharts
}

// Run scripts that a component has provided
func runComponentScripts(scripts []string, componentScript types.ZarfComponentScripts) {
	for _, script := range scripts {
		loopScriptUntilSuccess(script, componentScript)
	}
}

// Move files onto the host of the machine performing the deployment
func processComponentFiles(componentFiles []types.ZarfFile, sourceLocation, tempPathBase string) {
	var spinner message.Spinner
	if len(componentFiles) > 0 {
		spinner = *message.NewProgressSpinner("Copying %d files", len(componentFiles))
		defer spinner.Stop()
	}

	for index, file := range componentFiles {
		spinner.Updatef("Loading %s", file.Target)
		sourceFile := filepath.Join(sourceLocation, strconv.Itoa(index))

		// If a shasum is specified check it again on deployment as well
		if file.Shasum != "" {
			spinner.Updatef("Validating SHASUM for %s", file.Target)
			utils.ValidateSha256Sum(file.Shasum, sourceFile)
		}

		// Replace temp target directories
		file.Target = strings.Replace(file.Target, "###ZARF_TEMP###", tempPathBase, 1)

		// Copy the file to the destination
		spinner.Updatef("Saving %s", file.Target)
		err := copy.Copy(sourceFile, file.Target)
		if err != nil {
			spinner.Fatalf(err, "Unable to copy the contents of %s", file.Target)
		}

		// Loop over all symlinks and create them
		for _, link := range file.Symlinks {
			spinner.Updatef("Adding symlink %s->%s", link, file.Target)
			// Try to remove the filepath if it exists
			_ = os.RemoveAll(link)
			// Make sure the parent directory exists
			_ = utils.CreateFilePath(link)
			// Create the symlink
			err := os.Symlink(file.Target, link)
			if err != nil {
				spinner.Fatalf(err, "Unable to create the symbolic link %s -> %s", link, file.Target)
			}
		}

		// Cleanup now to reduce disk pressure
		_ = os.RemoveAll(sourceFile)
	}
	spinner.Success()

}

// Fetch the current ZarfState from the k8s cluster and generate a valueTemplate from the state values
func getUpdatedValueTemplate(component types.ZarfComponent) template.Values {
	// If we are touching K8s, make sure we can talk to it once per deployment
	spinner := message.NewProgressSpinner("Loading the Zarf State from the Kubernetes cluster")
	defer spinner.Stop()

	state, err := k8s.LoadZarfState()
	if err != nil {
		spinner.Fatalf(err, "Unable to load the Zarf State from the Kubernetes cluster")
	}

	if state.Distro == "" {
		// If no distro the zarf secret did not load properly
		spinner.Fatalf(nil, "Unable to load the zarf/zarf-state secret, did you remember to run zarf init first?")
	}

	// Continue loading state data if it is valid
	config.InitState(state)
	valueTemplate := template.Generate()
	if len(component.Images) > 0 && state.Architecture != config.GetArch() {
		// If the package has images but the architectures don't match warn the user to avoid ugly hidden errors with image push/pull
		spinner.Fatalf(nil, "This package architecture is %s, but this cluster seems to be initialized with the %s architecture",
			config.GetArch(),
			state.Architecture)
	}

	spinner.Success()

	return valueTemplate
}

// Push all of the components images to the configured container registry
func pushImagesToRegistry(tempPath tempPaths, componentImages []string, addShasumToImg bool) {
	if len(componentImages) == 0 {
		return
	}

	// Try image push up to 3 times
	for retry := 0; retry < 3; retry++ {
		if err := images.PushToZarfRegistry(tempPath.images, componentImages, addShasumToImg); err != nil {
			message.Errorf(err, "Unable to push images to the Registry, retrying in 5 seconds...")
			time.Sleep(5 * time.Second)
			continue
		} else {
			break
		}
	}
}

// Push all of the components git repos to the configured git server
func pushReposToRepository(reposPath string, repos []string) {
	if len(repos) == 0 {
		return
	}

	// Try repo push up to 3 times
	for retry := 0; retry < 3; retry++ {
		// Push all the repos from the extracted archive
		if err := git.PushAllDirectories(reposPath); err != nil {
			message.Errorf(err, "Unable to push repos to the Git Server, retrying in 5 seconds...")
			time.Sleep(5 * time.Second)
			continue
		} else {
			break
		}
	}
}

// Async'ly move data into a container running in a pod on the k8s cluster
func performDataInjections(waitGroup *sync.WaitGroup, componentPath componentPaths, dataInjections []types.ZarfDataInjection) {
	if len(dataInjections) > 0 {
		message.Info("Loading data injections")
	}

	for _, data := range dataInjections {
		waitGroup.Add(1)
		go handleDataInjection(waitGroup, data, componentPath)
	}
}

// Install all Helm charts and raw k8s manifests into the k8s cluster
func installChartAndManifests(componentPath componentPaths, component types.ZarfComponent) []types.InstalledChart {
	installedCharts := []types.InstalledChart{}

	for _, chart := range component.Charts {
		// zarf magic for the value file
		for idx := range chart.ValuesFiles {
			chartValueName := helm.StandardName(componentPath.values, chart) + "-" + strconv.Itoa(idx)
			valueTemplate.Apply(component, chartValueName)
		}

		// Generate helm templates to pass to gitops engine
		addedConnectStrings, installedChartName := helm.InstallOrUpgradeChart(helm.ChartOptions{
			BasePath:  componentPath.base,
			Chart:     chart,
			Component: component,
		})
		installedCharts = append(installedCharts, types.InstalledChart{Namespace: chart.Namespace, ChartName: installedChartName})

		// Iterate over any connectStrings and add to the main map
		for name, description := range addedConnectStrings {
			connectStrings[name] = description
		}
	}

	for _, manifest := range component.Manifests {
		for idx := range manifest.Kustomizations {
			// Move kustomizations to files now
			destination := fmt.Sprintf("kustomization-%s-%d.yaml", manifest.Name, idx)
			manifest.Files = append(manifest.Files, destination)
		}

		if manifest.Namespace == "" {
			// Helm gets sad when you don't provide a namespace even though we aren't using helm templating
			manifest.Namespace = corev1.NamespaceDefault
		}

		// Iterate over any connectStrings and add to the main map
		addedConnectStrings, installedChartName := helm.GenerateChart(componentPath.manifests, manifest, component)
		installedCharts = append(installedCharts, types.InstalledChart{Namespace: manifest.Namespace, ChartName: installedChartName})

		// Iterate over any connectStrings and add to the main map
		for name, description := range addedConnectStrings {
			connectStrings[name] = description
		}
	}

	return installedCharts
}

func writeSBOMFiles(sbomViewFiles []string) error {
	// Check if we even have any SBOM files to process
	if len(sbomViewFiles) == 0 {
		return nil
	}

	// Cleanup any failed prior removals
	_ = os.RemoveAll(config.ZarfSBOMDir)

	// Create the directory again
	err := utils.CreateDirectory(config.ZarfSBOMDir, 0755)
	if err != nil {
		return err
	}

	// Write each of the sbom files
	for _, file := range sbomViewFiles {
		// Our file copy lib explodes on these files for some reason...
		data, err := os.ReadFile(file)
		if err != nil {
			message.Fatalf(err, "Unable to read the sbom-viewer file %s", file)
		}
		dst := filepath.Join(config.ZarfSBOMDir, filepath.Base(file))
		err = os.WriteFile(dst, data, 0644)
		if err != nil {
			message.Debugf("Unable to write the sbom-viewer file %s", dst)
			return err
		}
	}

	return nil
}

func printTablesForDeployment(componentsToDeploy []types.ZarfComponent) {
	// If not init config, print the application connection table
	if !config.IsZarfInitConfig() {
		message.PrintConnectStringTable(connectStrings)
	} else {
		// otherwise, print the init config connection and passwords
		loginTableHeader := pterm.TableData{
			{"     Application", "Username", "Password", "Connect"},
		}

		loginTable := pterm.TableData{}
		if config.GetContainerRegistryInfo().InternalRegistry {
			loginTable = append(loginTable, pterm.TableData{{"     Registry", config.GetContainerRegistryInfo().PushUsername, config.GetContainerRegistryInfo().PushPassword, "zarf connect registry"}}...)
		}

		for _, component := range componentsToDeploy {
			// Show message if including logging stack
			if component.Name == "logging" {
				loginTable = append(loginTable, pterm.TableData{{"     Logging", "zarf-admin", config.GetState().LoggingSecret, "zarf connect logging"}}...)
			}
			// Show message if including git-server
			if component.Name == "git-server" {
				loginTable = append(loginTable, pterm.TableData{
					{"     Git", config.GetGitServerInfo().PushUsername, config.GetState().GitServer.PushPassword, "zarf connect git"},
					{"     Git (read-only)", config.GetGitServerInfo().PullUsername, config.GetState().GitServer.PullPassword, "zarf connect git"},
				}...)
			}
		}

		if len(loginTable) > 0 {
			loginTable = append(loginTableHeader, loginTable...)
			_ = pterm.DefaultTable.WithHasHeader().WithData(loginTable).Render()
		}
	}
}

func packageUsesK8s() bool {
	for _, component := range config.GetComponents() {
		// If the component is using anything that depends on the cluster, return true
		if len(component.Charts) > 0 ||
			len(component.Images) > 0 ||
			len(component.Repos) > 0 ||
			len(component.Manifests) > 0 {
			return true
		}
	}
	return false
}
