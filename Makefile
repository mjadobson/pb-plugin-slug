POCKETBASE_VERSION := v0.42.3
XPB_VERSION := v0.1.0
BASE_PLUGIN ?= github.com/mjadobson/pb-plugin-base@v0.3.0
PLUGIN_MODULE := github.com/mjadobson/pb-plugin-slug

.PHONY: build-pocketbase-binary
build-pocketbase-binary:
	go run github.com/pocketbuilds/xpb/cmd/xpb@$(XPB_VERSION) build $(POCKETBASE_VERSION) \
		--with github.com/pocketbuilds/xpb@$(XPB_VERSION) \
		--with "$(BASE_PLUGIN)" \
		--with "$(PLUGIN_MODULE)=$(CURDIR)" \
		--ldflag "-X ganigeorgiev.com/pocketbase/pocketbase.Version=$(POCKETBASE_VERSION)" \
		--output "$(CURDIR)/pocketbase"
