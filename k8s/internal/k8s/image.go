package k8s

import "strings"

// image is an image reference as the pods pull it: a reference with no registry host belongs to the registry the
// environment names (image_registry, e.g. localhost:5000 for a kind cluster with a local registry); with none named
// it stays as it is.
func (in *Installation) image(ref string) string {
	reg := strings.TrimSuffix(in.setting("image_registry", ""), "/")
	if len(reg) == 0 || len(ref) == 0 {
		return ref
	}
	first, _, slash := strings.Cut(ref, "/")
	if slash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return ref // it names its registry
	}
	return reg + "/" + ref
}
