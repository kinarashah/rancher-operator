package fleetcluster

import (
	"context"
	"errors"

	fleet "github.com/rancher/fleet/pkg/apis/fleet.cattle.io/v1alpha1"
	v1 "github.com/rancher/rancher-operator/pkg/apis/rancher.cattle.io/v1"
	"github.com/rancher/rancher-operator/pkg/clients"
	mgmtcontrollers "github.com/rancher/rancher-operator/pkg/generated/controllers/management.cattle.io/v3"
	"github.com/rancher/rancher-operator/pkg/settings"
	mgmt "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/wrangler/pkg/apply"
	"github.com/rancher/wrangler/pkg/generic"
	"github.com/rancher/wrangler/pkg/relatedresource"
	"github.com/rancher/wrangler/pkg/yaml"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

var (
	clusterName = "fleet.cattle.io/cluster-name"
)

type handler struct {
	settings mgmtcontrollers.SettingCache
	clusters mgmtcontrollers.ClusterClient
	apply    apply.Apply
}

func Register(ctx context.Context, clients *clients.Clients) {
	h := &handler{
		settings: clients.Management.Setting().Cache(),
		clusters: clients.Management.Cluster(),
		apply:    clients.Apply.WithCacheTypes(clients.Cluster.Cluster()),
	}

	clients.Management.Cluster().OnChange(ctx, "fleet-cluster-label", h.addLabel)
	mgmtcontrollers.RegisterClusterGeneratingHandler(ctx,
		clients.Management.Cluster(),
		clients.Apply.
			WithCacheTypes(clients.Fleet.Cluster(),
				clients.Cluster.Cluster()),
		"",
		"fleet-cluster",
		h.createCluster,
		nil,
	)

	relatedresource.WatchClusterScoped(ctx, "fleet-cluster-resolver", func(namespace, name string, obj runtime.Object) ([]relatedresource.Key, error) {
		owner, err := h.apply.FindOwner(obj)
		if err != nil {
			// ignore error
			return nil, nil
		}
		if c, ok := owner.(*v1.Cluster); ok {
			return []relatedresource.Key{{
				Namespace: c.Namespace,
				Name:      c.Name,
			}}, nil
		}
		return nil, nil
	}, clients.Management.Cluster(), clients.Cluster.Cluster())
}

func (h *handler) addLabel(key string, cluster *mgmt.Cluster) (*mgmt.Cluster, error) {
	if cluster == nil {
		return cluster, nil
	}

	if cluster.Spec.Internal && cluster.Spec.FleetWorkspaceName == "" {
		cluster = cluster.DeepCopy()
		cluster.Spec.FleetWorkspaceName = "fleet-local"
		return h.clusters.Update(cluster)
	} else if cluster.Spec.Internal {
		return cluster, nil
	}

	if cluster.Spec.FleetWorkspaceName == "" {
		def, err := settings.Get(h.settings, "fleet-default-workspace-name")
		if err != nil {
			return nil, err
		}

		if def == "" {
			return cluster, nil
		}

		cluster = cluster.DeepCopy()
		cluster.Spec.FleetWorkspaceName = def
		cluster, err = h.clusters.Update(cluster)
		if err != nil {
			return nil, err
		}
	}

	if cluster.Spec.FleetWorkspaceName == "" {
		return cluster, nil
	}

	if cluster.Labels[clusterName] != cluster.Name {
		cluster = cluster.DeepCopy()
		if cluster.Labels == nil {
			cluster.Labels = map[string]string{}
		}
		cluster.Labels[clusterName] = cluster.Name
		return h.clusters.Update(cluster)
	}

	return cluster, nil
}

func (h *handler) createCluster(cluster *mgmt.Cluster, status mgmt.ClusterStatus) ([]runtime.Object, mgmt.ClusterStatus, error) {
	if cluster.Spec.FleetWorkspaceName == "" ||
		cluster.Labels[clusterName] == "" ||
		cluster.Spec.Internal {
		return nil, status, nil
	}

	if !mgmt.ClusterConditionReady.IsTrue(cluster) {
		return nil, status, generic.ErrSkip
	}

	labels := yaml.CleanAnnotationsForExport(cluster.Labels)
	labels["management.cattle.io/cluster-name"] = cluster.Name
	if errs := validation.IsValidLabelValue(cluster.Spec.DisplayName); len(errs) == 0 {
		labels["management.cattle.io/cluster-display-name"] = cluster.Spec.DisplayName
	}

	var (
		secretName    = cluster.Name + "-kubeconfig"
		createCluster = true
		objs          []runtime.Object
	)

	if owningCluster, err := h.apply.FindOwner(cluster); errors.Is(err, apply.ErrOwnerNotFound) || errors.Is(err, apply.ErrNoInformerFound) {
	} else if err != nil {
		return nil, status, err
	} else if rCluster, ok := owningCluster.(*v1.Cluster); ok {
		if rCluster.Status.ClientSecretName == "" {
			return nil, status, generic.ErrSkip
		}
		createCluster = false
		secretName = rCluster.Status.ClientSecretName
	}

	if createCluster {
		objs = append(objs, &v1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cluster.Name,
				Namespace: cluster.Spec.FleetWorkspaceName,
				Labels:    labels,
			},
			Spec: v1.ClusterSpec{
				ReferencedConfig: &v1.ReferencedConfig{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							clusterName: cluster.Name,
						},
					},
				},
			},
		})
	}

	objs = append(objs, &fleet.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Spec.FleetWorkspaceName,
			Labels:    labels,
		},
		Spec: fleet.ClusterSpec{
			KubeConfigSecret: secretName,
		},
	})

	return objs, status, nil
}
