# SD-WAN CPE Data Plane - Top Level Makefile

.PHONY: all plugin controller test test-vbond test-frr clean docker

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

# vBOND 控制面 acceptance（复用 srwg-* 容器所在的 docker_net_ctrl）
test-vbond:
	@controller/test/scripts/test-vbond.sh

# sr-frr 把控制面 RIB 导出到 FRR 旁路表的 e2e（一次性 FRR 10.7.1 容器）
test-frr:
	@test/scripts/test-frr-export.sh

docker:
	@echo "Building Docker environment..."
	cd $(TEST_DIR)/docker && docker-compose build

clean:
	$(MAKE) -C $(VPP_PLUGIN_DIR) clean
	cd $(CONTROLLER_DIR) && go clean ./...
