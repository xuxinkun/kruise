package uniteddeployment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	apps "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/controller/history"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	appsalphav1 "github.com/openkruise/kruise/pkg/apis/apps/v1alpha1"
	"github.com/openkruise/kruise/pkg/util/refmanager"
)

// ControllerRevisionHashLabel is the label used to indicate the hash value of a ControllerRevision's Data.
const ControllerRevisionHashLabel = "controller.kubernetes.io/hash"

func (r *ReconcileUnitedDeployment) controlledHistories(ud *appsalphav1.UnitedDeployment) ([]*apps.ControllerRevision, error) {
	// List all histories to include those that don't match the selector anymore
	// but have a ControllerRef pointing to the controller.
	selector, err := metav1.LabelSelectorAsSelector(ud.Spec.Selector)
	if err != nil {
		return nil, err
	}
	histories := &apps.ControllerRevisionList{}
	err = r.Client.List(context.TODO(), &client.ListOptions{LabelSelector: selector}, histories)
	if err != nil {
		return nil, err
	}
	klog.V(1).Infof("List controller revision of UnitedDeployment %s/%s: count %d\n", ud.Namespace, ud.Name, len(histories.Items))

	// Use ControllerRefManager to adopt/orphan as needed.
	cm, err := refmanager.New(r.Client, ud.Spec.Selector, ud, r.scheme)
	if err != nil {
		return nil, err
	}

	mts := make([]metav1.Object, len(histories.Items))
	for i, pod := range histories.Items {
		mts[i] = pod.DeepCopy()
	}
	claims, err := cm.ClaimOwnedObjects(mts)
	if err != nil {
		return nil, err
	}

	claimHistories := make([]*apps.ControllerRevision, len(claims))
	for i, mt := range claims {
		claimHistories[i] = mt.(*apps.ControllerRevision)
	}

	return claimHistories, nil
}

func (r *ReconcileUnitedDeployment) constructUnitedDeploymentRevisions(ud *appsalphav1.UnitedDeployment) (*apps.ControllerRevision, *apps.ControllerRevision, *[]*apps.ControllerRevision, int32, error) {
	var currentRevision, updateRevision *apps.ControllerRevision
	revisions, err := r.controlledHistories(ud)
	if err != nil {
		if ud.Status.CollisionCount == nil {
			return currentRevision, updateRevision, nil, 0, err
		}
		return currentRevision, updateRevision, nil, *ud.Status.CollisionCount, err
	}

	history.SortControllerRevisions(revisions)
	cleanedRevision, err := r.cleanExpiredRevision(ud, &revisions)
	if err != nil {
		if ud.Status.CollisionCount == nil {
			return currentRevision, updateRevision, nil, 0, err
		}
		return currentRevision, updateRevision, nil, *ud.Status.CollisionCount, err
	}
	revisions = *cleanedRevision

	// Use a local copy of set.Status.CollisionCount to avoid modifying set.Status directly.
	// This copy is returned so the value gets carried over to set.Status in updateStatefulSet.
	var collisionCount int32
	if ud.Status.CollisionCount != nil {
		collisionCount = *ud.Status.CollisionCount
	}

	// create a new revision from the current set
	updateRevision, err = r.newRevision(ud, nextRevision(revisions), &collisionCount)
	if err != nil {
		return nil, nil, nil, collisionCount, err
	}

	// find any equivalent revisions
	equalRevisions := history.FindEqualRevisions(revisions, updateRevision)
	equalCount := len(equalRevisions)
	revisionCount := len(revisions)

	if equalCount > 0 && history.EqualRevision(revisions[revisionCount-1], equalRevisions[equalCount-1]) {
		// if the equivalent revision is immediately prior the update revision has not changed
		updateRevision = revisions[revisionCount-1]
	} else if equalCount > 0 {
		// if the equivalent revision is not immediately prior we will roll back by incrementing the
		// Revision of the equivalent revision
		equalRevisions[equalCount-1].Revision = updateRevision.Revision
		err := r.Client.Update(context.TODO(), equalRevisions[equalCount-1])
		if err != nil {
			return nil, nil, nil, collisionCount, err
		}
		updateRevision = equalRevisions[equalCount-1]
	} else {
		//if there is no equivalent revision we create a new one
		updateRevision, err = r.createControllerRevision(ud, updateRevision, &collisionCount)
		if err != nil {
			return nil, nil, nil, collisionCount, err
		}
	}

	// attempt to find the revision that corresponds to the current revision
	for i := range revisions {
		if revisions[i].Name == ud.Status.CurrentRevision {
			currentRevision = revisions[i]
		}
	}

	// if the current revision is nil we initialize the history by setting it to the update revision
	if currentRevision == nil {
		currentRevision = updateRevision
	}

	return currentRevision, updateRevision, &revisions, collisionCount, nil
}

func (r *ReconcileUnitedDeployment) cleanExpiredRevision(ud *appsalphav1.UnitedDeployment, sortedRevisions *[]*apps.ControllerRevision) (*[]*apps.ControllerRevision, error) {
	exceedNum := len(*sortedRevisions) - int(*ud.Spec.RevisionHistoryLimit)
	if exceedNum <= 0 {
		return sortedRevisions, nil
	}

	for i, revision := range *sortedRevisions {
		if i >= exceedNum {
			break
		}

		if err := r.Client.Delete(context.TODO(), revision); err != nil {
			return sortedRevisions, err
		}
	}
	cleanedRevisions := (*sortedRevisions)[exceedNum:]

	return &cleanedRevisions, nil
}

// createControllerRevision creates the controller revision owned by the parent.
func (r *ReconcileUnitedDeployment) createControllerRevision(parent metav1.Object, revision *apps.ControllerRevision, collisionCount *int32) (*apps.ControllerRevision, error) {
	if collisionCount == nil {
		return nil, fmt.Errorf("collisionCount should not be nil")
	}

	// Clone the input
	clone := revision.DeepCopy()

	var err error
	// Continue to attempt to create the revision updating the name with a new hash on each iteration
	for {
		hash := history.HashControllerRevision(revision, collisionCount)
		// Update the revisions name
		clone.Name = history.ControllerRevisionName(parent.GetName(), hash)
		err = r.Client.Create(context.TODO(), clone)
		if errors.IsAlreadyExists(err) {
			exists := &apps.ControllerRevision{}
			err := r.Client.Get(context.TODO(), client.ObjectKey{Namespace: parent.GetNamespace(), Name: clone.Name}, exists)
			if err != nil {
				return nil, err
			}
			if bytes.Equal(exists.Data.Raw, clone.Data.Raw) {
				return exists, nil
			}
			*collisionCount++
			continue
		}
		return clone, err
	}
}

// newRevision creates a new ControllerRevision containing a patch that reapplies the target state of set.
// The Revision of the returned ControllerRevision is set to revision. If the returned error is nil, the returned
// ControllerRevision is valid. StatefulSet revisions are stored as patches that re-apply the current state of set
// to a new StatefulSet using a strategic merge patch to replace the saved state of the new StatefulSet.
func (r *ReconcileUnitedDeployment) newRevision(ud *appsalphav1.UnitedDeployment, revision int64, collisionCount *int32) (*apps.ControllerRevision, error) {
	patch, err := getUnitedDeploymentPatch(ud)
	if err != nil {
		return nil, err
	}

	gvk, err := apiutil.GVKForObject(ud, r.scheme)
	if err != nil {
		return nil, err
	}

	cr, err := history.NewControllerRevision(ud,
		gvk,
		ud.Spec.Template.StatefulSetTemplate.Labels,
		runtime.RawExtension{Raw: patch},
		revision,
		collisionCount)
	if err != nil {
		return nil, err
	}
	cr.Namespace = ud.Namespace

	return cr, nil
}

// nextRevision finds the next valid revision number based on revisions. If the length of revisions
// is 0 this is 1. Otherwise, it is 1 greater than the largest revision's Revision. This method
// assumes that revisions has been sorted by Revision.
func nextRevision(revisions []*apps.ControllerRevision) int64 {
	count := len(revisions)
	if count <= 0 {
		return 1
	}
	return revisions[count-1].Revision + 1
}

func getUnitedDeploymentPatch(ud *appsalphav1.UnitedDeployment) ([]byte, error) {
	dsBytes, err := json.Marshal(ud)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	err = json.Unmarshal(dsBytes, &raw)
	if err != nil {
		return nil, err
	}
	objCopy := make(map[string]interface{})
	specCopy := make(map[string]interface{})

	// Create a patch of the UnitedDeployment that replaces spec.template
	spec := raw["spec"].(map[string]interface{})
	template := spec["template"].(map[string]interface{})
	specCopy["template"] = template
	template["$patch"] = "replace"
	objCopy["spec"] = specCopy
	patch, err := json.Marshal(objCopy)
	return patch, err
}
