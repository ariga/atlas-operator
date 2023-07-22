package controllers

import (
	"context"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
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

// hydrateCredentials hydrates the credentials with the password from the secret.
func hydrateCredentials(ctx context.Context, creds *dbv1alpha1.Credentials, r client.Reader, ns string) error {
	if creds.PasswordFrom.SecretKeyRef != nil {
		sec, err := getSecretValue(ctx, r, ns, *creds.PasswordFrom.SecretKeyRef)
		if err != nil {
			return transient(err)
		}
		creds.Password = sec
	}
	return nil
}
