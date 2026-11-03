#include "rockit_system.h"

#include <mutex>

extern "C" {
#include "rk_mpi_sys.h"
}

namespace aico {

namespace {

struct RockitSystemState {
    std::mutex mutex;
    unsigned int users = 0;
};

RockitSystemState& rockit_system_state() {
    // Keep the process-global lock alive through static destruction. The JPEG
    // encoder is also function-static and may release its final reference from
    // its destructor after other translation units have begun teardown.
    static RockitSystemState* state = new RockitSystemState();
    return *state;
}

}  // namespace

bool acquire_rockit_system() {
    RockitSystemState& state = rockit_system_state();
    std::lock_guard<std::mutex> lock(state.mutex);
    if (state.users == 0 && RK_MPI_SYS_Init() != RK_SUCCESS) {
        return false;
    }
    ++state.users;
    return true;
}

void release_rockit_system() {
    RockitSystemState& state = rockit_system_state();
    std::lock_guard<std::mutex> lock(state.mutex);
    if (state.users == 0) {
        return;
    }
    --state.users;
    if (state.users == 0) {
        RK_MPI_SYS_Exit();
    }
}

}  // namespace aico
