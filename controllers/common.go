package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getSecretValue gets the value of the given secret key selector.
func getSecretValue(
	ctx context.Context,
	r client.Reader,
	ns string,
	selector corev1.SecretKeySelector,
) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: selector.Name}, secret); err != nil {
		return "", transient(err)
	}
	us := string(secret.Data[selector.Key])
	return us, nil
}
