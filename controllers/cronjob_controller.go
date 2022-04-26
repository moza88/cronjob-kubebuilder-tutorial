/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context" 
	"fmt" 
	"sort" 
	"time"  
	"github.com/robfig/cron"    
	kbatch "k8s.io/api/batch/v1" 
	corev1 "k8s.io/api/core/v1" 
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"      
	"k8s.io/apimachinery/pkg/runtime" 
	ref "k8s.io/client-go/tools/reference" 
	ctrl "sigs.k8s.io/controller-runtime"  
	"sigs.k8s.io/controller-runtime/pkg/client"  
	"sigs.k8s.io/controller-runtime/pkg/log"  
	batchv1 "tutorial.cronjob.io/project/api/v1"
)

/*
We'll mock out the clock to make it easier to jump around in time while testing,
the "real" clock just calls `time.Now`.
*/
type realClock struct{}

func (_ realClock) Now() time.Time { return time.Now() }

// clock knows how to get the current time.
// It can be used to fake out timing for testing.
type Clock interface {
   Now() time.Time
}
// +kubebuilder:docs-gen:collapse=Clock

// CronJobReconciler reconciles a CronJob object
type CronJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock
}

//+kubebuilder:rbac:groups=batch.cronjob.kubebuilder.io,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch.cronjob.kubebuilder.io,resources=cronjobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch.cronjob.kubebuilder.io,resources=cronjobs/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get

var (
    scheduledTimeAnnotation = "batch.cronjob.kubebuilder.io/scheduled-at"
)

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CronJob object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log = log.FromContext(ctx)

	/**
		1. Load the CronJob by Name
	*/
	var cronJob batchv1.CronJob
	if err := r.Get(ctx, req.NamespacedName, &cronJob); err != nil {
		log.Error(err, "unable to fetch CronJob")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	 }

	/**
	 	2. List all Active Jobs & Update the Status
	*/

	var childJobs kbatch.JobList
	if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		log.Error(err, "unable to list child Jobs")
		return ctrl.Result{}, err
	}

    // find the active list of jobs
    var activeJobs []*kbatch.Job
    var successfulJobs []*kbatch.Job
    var failedJobs []*kbatch.Job
    var mostRecentTime *time.Time // find the last run so we can update the status

	/**
		Action if Job is Finished
	*/
	isJobFinished := func(job *kbatch.Job) (bool, kbatch.JobConditionType) {
		for _, c := range job.Status.Conditions {
			if (c.Type == kbatch.JobComplete || c.Type == kbatch.JobFailed) && c.Status == corev1.ConditionTrue {
				return true, c.Type
			}
		}

		return false, ""
	}
	// +kubebuilder:docs-gen:collapse=isJobFinished

	/**
		Extract Schedule Time from Annotation
	*/
	getScheduledTimeForJob := func(job *kbatch.Job) (*time.Time, error) {
        timeRaw := job.Annotations[scheduledTimeAnnotation]
        if len(timeRaw) == 0 {
            return nil, nil
        }

        timeParsed, err := time.Parse(time.RFC3339, timeRaw)
        if err != nil {
            return nil, err
        }
        return &timeParsed, nil
    }
    for i, job := range childJobs.Items {
        _, finishedType := isJobFinished(&job)
        switch finishedType {
        case "": // ongoing
            activeJobs = append(activeJobs, &childJobs.Items[i])
        case kbatch.JobFailed:
            failedJobs = append(failedJobs, &childJobs.Items[i])
        case kbatch.JobComplete:
            successfulJobs = append(successfulJobs, &childJobs.Items[i])
        }

        // We'll store the launch time in an annotation, so we'll reconstitute that from
        // the active jobs themselves.
        scheduledTimeForJob, err := getScheduledTimeForJob(&job)
        if err != nil {
            log.Error(err, "unable to parse schedule time for child job", "job", &job)
            continue
        }
        if scheduledTimeForJob != nil {
            if mostRecentTime == nil {
                mostRecentTime = scheduledTimeForJob
            } else if mostRecentTime.Before(*scheduledTimeForJob) {
                mostRecentTime = scheduledTimeForJob
            }
        }
    }

    if mostRecentTime != nil {
        cronJob.Status.LastScheduleTime = &metav1.Time{Time: *mostRecentTime}
    } else {
        cronJob.Status.LastScheduleTime = nil
    }
    cronJob.Status.Active = nil
    for _, activeJob := range activeJobs {
        jobRef, err := ref.GetReference(r.Scheme, activeJob)
        if err != nil {
            log.Error(err, "unable to make reference to active job", "job", activeJob)
            continue
        }
        cronJob.Status.Active = append(cronJob.Status.Active, *jobRef)
    }

	log.V(1).Info("job count", "active jobs", len(activeJobs), "successful jobs", len(successfulJobs), "failed jobs", len(failedJobs))

	if err := r.Status().Update(ctx, &cronJob); err != nil {
		log.Error(err, "unable to update CronJob status")
		return ctrl.Result{}, err
	}

	/**
		3. Clean up Old Jobs
	*/
	if cronJob.Spec.FailedJobsHistoryLimit != nil {
		sort.Slice(failedJobs, func(i, j int) bool {
			if failedJobs[i].Status.StartTime == nil {
				return failedJobs[j].Status.StartTime != nil
			}
			return failedJobs[i].Status.StartTime.Before(failedJobs[j].Status.StartTime)
		})
		for i, job := range failedJobs {
			if int32(i) >= int32(len(failedJobs))-*cronJob.Spec.FailedJobsHistoryLimit {
				break
			}
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete old failed job", "job", job)
			} else {
				log.V(0).Info("deleted old failed job", "job", job)
			}
		}
	}

	/**
		4. Check for Suspension
	*/
	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		log.V(1).Info("Cronjob Suspended, Skipping")
		return ctrl.Result{}, nil
	}

	/**
		5. Get Next Scheduled Run
	*/
	getNextSchedule := func(cronJob *batchv1.CronJob, now time.Time) (lastMissed time.Time, next time.Time, err error) {
		sched, err := cron.ParseStandard(cronJob.Spec.Schedule)
		if err != nil {
		   return time.Time{}, time.Time{}, fmt.Errorf("Unparseable schedule %q: %v", cronJob.Spec.Schedule, err)
		}
	 
		var earliestTime time.Time
		if cronJob.Status.LastScheduleTime != nil {
		   earliestTime = cronJob.Status.LastScheduleTime.Time
		} else {
		   earliestTime = cronJob.ObjectMeta.CreationTimestamp.Time
		}
		if cronJob.Spec.StartingDeadlineSeconds != nil {
		   // controller is not going to schedule anything below this point
		   schedulingDeadline := now.Add(-time.Second * time.Duration(*cronJob.Spec.StartingDeadlineSeconds))
	 
		   if schedulingDeadline.After(earliestTime) {
			  earliestTime = schedulingDeadline
		   }
		}
		if earliestTime.After(now) {
		   return time.Time{}, sched.Next(now), nil
		}
	 
		starts := 0
		for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
		   lastMissed = t
		   starts++
		   if starts > 100 {
			  return time.Time{}, time.Time{}, fmt.Errorf("Too many missed start times (> 100). Set or decrease .spec.startingDeadlineSeconds or check clock skew.")
		   }
		}
		return lastMissed, sched.Next(now), nil
	 }
	 // +kubebuilder:docs-gen:collapse=getNextSchedule
	 missedRun, nextRun, err := getNextSchedule(&cronJob, r.Now())
if err != nil {
   log.Error(err, "unable to figure out CronJob schedule")
   return ctrl.Result{}, nil
}

scheduledResult := ctrl.Result{RequeueAfter: nextRun.Sub(r.Now())} 
log = log.WithValues("now", r.Now(), "next run", nextRun)

/*
   ### 6: Run a new job if it's on schedule, not past the deadline, and not blocked by our concurrency policy
   If we've missed a run, and we're still within the deadline to start it, we'll need to run a job.
*/
if missedRun.IsZero() {
   log.V(1).Info("no upcoming scheduled times, sleeping until next")
   return scheduledResult, nil
}

// make sure we're not too late to start the run
log = log.WithValues("current run", missedRun)
tooLate := false
if cronJob.Spec.StartingDeadlineSeconds != nil {
   tooLate = missedRun.Add(time.Duration(*cronJob.Spec.StartingDeadlineSeconds) * time.Second).Before(r.Now())
}
if tooLate {
   log.V(1).Info("missed starting deadline for last run, sleeping till next")
   return scheduledResult, nil
}
if cronJob.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent && len(activeJobs) > 0 {
   log.V(1).Info("concurrency policy blocks concurrent runs, skipping", "num active", len(activeJobs))
   return scheduledResult, nil
}

if cronJob.Spec.ConcurrencyPolicy == batchv1.ReplaceConcurrent {
   for _, activeJob := range activeJobs {
      // we don't care if the job was already deleted
      if err := r.Delete(ctx, activeJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
         log.Error(err, "unable to delete active job", "job", activeJob)
         return ctrl.Result{}, err
      }
   }
}
constructJobForCronJob := func(cronJob *batchv1.CronJob, scheduledTime time.Time) (*kbatch.Job, error) {
	name := fmt.Sprintf("%s-%d", cronJob.Name, scheduledTime.Unix())
 
	job := &kbatch.Job{
	   ObjectMeta: metav1.ObjectMeta{
		  Labels:      make(map[string]string),
		  Annotations: make(map[string]string),
		  Name:        name,
		  Namespace:   cronJob.Namespace,
	   },
	   Spec: *cronJob.Spec.JobTemplate.Spec.DeepCopy(),
	}
	for k, v := range cronJob.Spec.JobTemplate.Annotations {
	   job.Annotations[k] = v
	}
	job.Annotations[scheduledTimeAnnotation] = scheduledTime.Format(time.RFC3339)
	for k, v := range cronJob.Spec.JobTemplate.Labels {
	   job.Labels[k] = v
	}
	if err := ctrl.SetControllerReference(cronJob, job, r.Scheme); err != nil {
	   return nil, err
	}
 
	return job, nil
 }
 // +kubebuilder:docs-gen:collapse=constructJobForCronJob
 // actually make the job...
 job, err := constructJobForCronJob(&cronJob, missedRun)
 if err != nil {
	log.Error(err, "unable to construct job from template")
	// don't bother requeuing until we get a change to the spec
	return scheduledResult, nil
 }
 
 // ...and create it on the cluster
 if err := r.Create(ctx, job); err != nil {
	log.Error(err, "unable to create Job for CronJob", "job", job)
	return ctrl.Result{}, err
 }
 
 log.V(1).Info("created Job for CronJob run", "job", job)

 return scheduledResult, nil
}

// SetupWithManager sets up the controller with the Manager.
var (
    jobOwnerKey = ".metadata.controller"
    apiGVStr    = batchv1.GroupVersion.String()
)

func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
    // set up a real clock, since we're not in a test
    if r.Clock == nil {
        r.Clock = realClock{}
    }

    if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kbatch.Job{}, jobOwnerKey, func(rawObj client.Object) []string {
        // grab the job object, extract the owner...
        job := rawObj.(*kbatch.Job)
        owner := metav1.GetControllerOf(job)
        if owner == nil {
            return nil
        }
        // ...make sure it's a CronJob...
        if owner.APIVersion != apiGVStr || owner.Kind != "CronJob" {
            return nil
        }

        // ...and if so, return it
        return []string{owner.Name}
    }); err != nil {
        return err
    }

    return ctrl.NewControllerManagedBy(mgr).
        For(&batchv1.CronJob{}).
        Owns(&kbatch.Job{}).
        Complete(r)
}