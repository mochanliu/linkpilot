# SD-WAN CPE Data Plane - Top Level Makefile

.PHONY: all plugin controller test clean docker

VPP_PLUGIN_DIR = vpp-plugin
CONTROLLER_DIR = controller
TEST_DIR = test

all: plugin controller

plugin:
	@echo "Building VPP plugin..."
	$(MAKE) -C $(VPP_PLUGIN_DIR)

controller:
	@echo "Building Go controller..."
	cd $(CONTROLLER_DIR) && go build ./...

test: test-unit test-integration

test-unit:
	@echo "Running unit tests..."
	$(MAKE) -C $(TEST_DIR)/unit test

test-integration:
	@echo "Running integration tests..."
	cd $(TEST_DIR)/docker && docker-compose up -d && sleep 5 && ../scripts/run-tests.sh

docker:
	@echo "Building Docker environment..."
	cd $(TEST_DIR)/docker && docker-compose build

clean:
	$(MAKE) -C $(VPP_PLUGIN_DIR) clean
	cd $(CONTROLLER_DIR) && go clean ./...
