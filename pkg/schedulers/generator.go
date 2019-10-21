package schedulers

import (
	"io/ioutil"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/jenkins-x/jx/pkg/log"

	jenkinsv1 "github.com/jenkins-x/jx/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx/pkg/client/clientset/versioned"
	"github.com/jenkins-x/lighthouse/pkg/prow/config"
	"github.com/jenkins-x/lighthouse/pkg/prow/plugins"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GenerateProw will generate the prow config for the namespace
func GenerateProw(gitOps bool, autoApplyConfigUpdater bool, jxClient versioned.Interface, namespace string, teamSchedulerName string, devEnv *jenkinsv1.Environment, loadSchedulerResourcesFunc func(versioned.Interface, string) (map[string]*jenkinsv1.Scheduler, *jenkinsv1.SourceRepositoryGroupList, *jenkinsv1.SourceRepositoryList, error)) (*config.Config,
	*plugins.Configuration, error) {
	if loadSchedulerResourcesFunc == nil {
		loadSchedulerResourcesFunc = loadSchedulerResources
	}
	schedulers, sourceRepoGroups, sourceRepos, err := loadSchedulerResourcesFunc(jxClient, namespace)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "loading scheduler resources")
	}
	if sourceRepos == nil || len(sourceRepos.Items) < 1 {
		return nil, nil, errors.New("No source repository resources were found")
	}
	defaultScheduler := schedulers[teamSchedulerName]
	leaves := make([]*SchedulerLeaf, 0)
	for _, sourceRepo := range sourceRepos.Items {
		applicableSchedulers := []*jenkinsv1.SchedulerSpec{}
		// Apply config-updater to devEnv
		applicableSchedulers = addConfigUpdaterToDevEnv(gitOps, autoApplyConfigUpdater, applicableSchedulers, devEnv, &sourceRepo.Spec)
		// Apply repo scheduler
		applicableSchedulers = addRepositoryScheduler(sourceRepo, schedulers, applicableSchedulers)
		// Apply project schedulers
		applicableSchedulers = addProjectSchedulers(sourceRepoGroups, sourceRepo, schedulers, applicableSchedulers)
		// Apply team scheduler
		applicableSchedulers = addTeamScheduler(teamSchedulerName, defaultScheduler, applicableSchedulers)
		if len(applicableSchedulers) < 1 {
			continue
		}
		merged, err := Build(applicableSchedulers)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "building scheduler")
		}
		leaves = append(leaves, &SchedulerLeaf{
			Repo:          sourceRepo.Spec.Repo,
			Org:           sourceRepo.Spec.Org,
			SchedulerSpec: merged,
		})
		if err != nil {
			return nil, nil, errors.Wrapf(err, "building prow config")
		}
	}
	cfg, plugs, err := BuildProwConfig(leaves)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "building prow config")
	}
	if cfg != nil {
		cfg.PodNamespace = namespace
		//cfg.ProwJobNamespace = namespace
	}
	return cfg, plugs, nil
}

func loadSchedulerResources(jxClient versioned.Interface, namespace string) (map[string]*jenkinsv1.Scheduler, *jenkinsv1.SourceRepositoryGroupList, *jenkinsv1.SourceRepositoryList, error) {
	schedulers, err := jxClient.JenkinsV1().Schedulers(namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, nil, nil, errors.WithStack(err)
	}
	if len(schedulers.Items) == 0 {
		return nil, nil, nil, errors.New("No pipeline schedulers are configured")
	}
	lookup := make(map[string]*jenkinsv1.Scheduler)
	for _, item := range schedulers.Items {
		lookup[item.Name] = item.DeepCopy()
	}
	// Process Schedulers linked to SourceRepositoryGroups
	sourceRepoGroups, err := jxClient.JenkinsV1().SourceRepositoryGroups(namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, nil, nil, errors.Wrapf(err, "Error finding source repository groups")
	}
	// Process Schedulers linked to SourceRepositoryGroups
	sourceRepos, err := jxClient.JenkinsV1().SourceRepositories(namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, nil, nil, errors.Wrapf(err, "Error finding source repositories")
	}
	return lookup, sourceRepoGroups, sourceRepos, nil
}

//cleanupExistingProwConfig Removes config that we do not currently support
func cleanupExistingProwConfig(prowConfig *config.Config, pluginConfig *plugins.Configuration, sourceRepoMap map[string]*jenkinsv1.SourceRepository) {
	// Deck is not supported
	prowConfig.Deck = config.Deck{}
	// heart plugin is not supported
	pluginConfig.Heart = plugins.Heart{}
	queries := prowConfig.Tide.Queries[:0]
	for _, query := range prowConfig.Tide.Queries {
		repos := query.Repos[:0]
		for _, repo := range query.Repos {
			// We do not support tide queries for repos with not presubmits or postsubmits, ignore the dummy environment for now
			if sourceRepoMap[repo] != nil {
				repos = append(repos, repo)
			}
		}
		if len(repos) > 0 {
			query.Repos = repos
			queries = append(queries, query)
		}
	}
	prowConfig.Tide.Queries = queries

	pluginConfig.ConfigUpdater = plugins.ConfigUpdater{}
	triggers := make([]plugins.Trigger, 0)
	for _, trigger := range pluginConfig.Triggers {
		for _, repo := range trigger.Repos {
			// We do not support tide queries for repos with not presubmits or postsubmits, ignore the dummy environment for now
			if sourceRepoMap[repo] != nil {
				triggers = append(triggers, trigger)
			}
		}
	}
	pluginConfig.Triggers = triggers

	lgtms := make([]plugins.Lgtm, 0)
	for _, lgtm := range pluginConfig.Lgtm {
		for _, repo := range lgtm.Repos {
			// We do not support tide queries for repos with not presubmits or postsubmits, ignore the dummy environment for now
			if sourceRepoMap[repo] != nil {
				newLgtm := lgtm
				newLgtm.Repos = []string{repo}
				lgtms = append(lgtms, newLgtm)
			}
		}
	}
	pluginConfig.Lgtm = lgtms

	approves := make([]plugins.Approve, 0)
	for _, approve := range pluginConfig.Approve {
		for _, repo := range approve.Repos {
			if !strings.Contains(repo, "/") {
				// Expand the org in to repos for now
				for existingRepo := range sourceRepoMap {
					if strings.HasPrefix(existingRepo, repo+"/") {
						newApprove := approve
						newApprove.Repos = []string{existingRepo}
						approves = append(approves, newApprove)
					}
				}
			} else {
				// We do not support tide queries for repos with not presubmits or postsubmits, ignore the dummy environment for now
				if sourceRepoMap[repo] != nil {
					approves = append(approves, approve)
				}
			}
		}
	}
	pluginConfig.Approve = approves

	// Branch org protection policy out in to repos
	for org := range prowConfig.BranchProtection.Orgs {
		protectionOrg := prowConfig.BranchProtection.Orgs[org]
		if protectionOrg.Repos == nil {
			protectionOrg.Repos = make(map[string]config.Repo)
			replacedOrgPolicy := false
			for existingRepo := range sourceRepoMap {
				if strings.HasPrefix(existingRepo, org+"/") {
					orgRepo := strings.Split(existingRepo, "/")
					repoPolicy := config.Repo{
						Policy: protectionOrg.Policy,
					}
					protectionOrg.Repos[orgRepo[1]] = repoPolicy
					prowConfig.BranchProtection.Orgs[org] = protectionOrg
					replacedOrgPolicy = true
				}
			}
			if replacedOrgPolicy {
				protectionOrg := prowConfig.BranchProtection.Orgs[org]
				protectionOrg.Policy = config.Policy{}
				prowConfig.BranchProtection.Orgs[org] = protectionOrg
			}
		}
	}

	for repo, plugins := range pluginConfig.ExternalPlugins {
		if plugins == nil {
			delete(pluginConfig.ExternalPlugins, repo)
			continue
		}
		if sourceRepoMap[repo] == nil {
			delete(pluginConfig.ExternalPlugins, repo)
		}
	}
	for repo := range pluginConfig.Plugins {
		if sourceRepoMap[repo] == nil {
			delete(pluginConfig.Plugins, repo)
		}
	}
}

func dumpProwConfigToFiles(prefix string, prowConfig *config.Config, pluginConfig *plugins.Configuration) {
	migratedConfigFile := prefix + "Config.yaml"
	cnfBytes, err := yaml.Marshal(prowConfig)
	if err != nil {
		log.Logger().Error("marshaling prow plugins config to yaml")
	}
	err = ioutil.WriteFile(migratedConfigFile, cnfBytes, 0600)
	if err != nil {
		log.Logger().Errorf("writing %s", migratedConfigFile)
	}
	log.Logger().Infof("Writing migrated config to %s", migratedConfigFile)
	migratedPluginsFile := prefix + "Plugins.yaml"
	plugsBytes, err := yaml.Marshal(pluginConfig)
	if err != nil {
		log.Logger().Error("marshaling prow plugins config to yaml")
	}
	err = ioutil.WriteFile(migratedPluginsFile, plugsBytes, 0600)
	if err != nil {
		log.Logger().Errorf("writing %s", migratedPluginsFile)
	}
	log.Logger().Infof("Writing migrated plugins to %s", migratedPluginsFile)
}

func addTeamScheduler(defaultSchedulerName string, defaultScheduler *jenkinsv1.Scheduler, applicableSchedulers []*jenkinsv1.SchedulerSpec) []*jenkinsv1.SchedulerSpec {
	if defaultScheduler != nil {
		applicableSchedulers = append([]*jenkinsv1.SchedulerSpec{&defaultScheduler.Spec}, applicableSchedulers...)
	} else {
		if defaultSchedulerName != "" {
			log.Logger().Warnf("A team pipeline scheduler named %s was configured but could not be found", defaultSchedulerName)
		}
	}
	return applicableSchedulers
}

func addRepositoryScheduler(sourceRepo jenkinsv1.SourceRepository, lookup map[string]*jenkinsv1.Scheduler, applicableSchedulers []*jenkinsv1.SchedulerSpec) []*jenkinsv1.SchedulerSpec {
	if sourceRepo.Spec.Scheduler.Name != "" {
		scheduler := lookup[sourceRepo.Spec.Scheduler.Name]
		if scheduler != nil {
			applicableSchedulers = append([]*jenkinsv1.SchedulerSpec{&scheduler.Spec}, applicableSchedulers...)
		} else {
			log.Logger().Warnf("A scheduler named %s is referenced by repository(%s) but could not be found", sourceRepo.Spec.Scheduler.Name, sourceRepo.Name)
		}
	}
	return applicableSchedulers
}

func addProjectSchedulers(sourceRepoGroups *jenkinsv1.SourceRepositoryGroupList, sourceRepo jenkinsv1.SourceRepository, lookup map[string]*jenkinsv1.Scheduler, applicableSchedulers []*jenkinsv1.SchedulerSpec) []*jenkinsv1.SchedulerSpec {
	if sourceRepoGroups != nil {
		for _, sourceGroup := range sourceRepoGroups.Items {
			for _, groupRepo := range sourceGroup.Spec.SourceRepositorySpec {
				if groupRepo.Name == sourceRepo.Name {
					if sourceGroup.Spec.Scheduler.Name != "" {
						scheduler := lookup[sourceGroup.Spec.Scheduler.Name]
						if scheduler != nil {
							applicableSchedulers = append([]*jenkinsv1.SchedulerSpec{&scheduler.Spec}, applicableSchedulers...)
						} else {
							log.Logger().Warnf("A scheduler named %s is referenced by repository group(%s) but could not be found", sourceGroup.Spec.Scheduler.Name, sourceGroup.Name)
						}
					}
				}
			}
		}
	}
	return applicableSchedulers
}

func addConfigUpdaterToDevEnv(gitOps bool, autoApplyConfigUpdater bool, applicableSchedulers []*jenkinsv1.SchedulerSpec, devEnv *jenkinsv1.Environment, sourceRepo *jenkinsv1.SourceRepositorySpec) []*jenkinsv1.SchedulerSpec {
	if gitOps && autoApplyConfigUpdater && strings.Contains(devEnv.Spec.Source.URL, sourceRepo.Org+"/"+sourceRepo.Repo) {
		maps := make(map[string]jenkinsv1.ConfigMapSpec)
		maps["env/prow/config.yaml"] = jenkinsv1.ConfigMapSpec{
			Name: "config",
		}
		maps["env/prow/plugins.yaml"] = jenkinsv1.ConfigMapSpec{
			Name: "plugins",
		}
		environmentUpdaterSpec := &jenkinsv1.SchedulerSpec{
			ConfigUpdater: &jenkinsv1.ConfigUpdater{
				Map: maps,
			},
			Plugins: &jenkinsv1.ReplaceableSliceOfStrings{
				Items: []string{"config-updater"},
			},
		}
		applicableSchedulers = append([]*jenkinsv1.SchedulerSpec{environmentUpdaterSpec}, applicableSchedulers...)
	}
	return applicableSchedulers
}
