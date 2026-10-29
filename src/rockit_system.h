#pragma once

namespace aico {

// Rockit exposes process-global media state. Keep all users in this process on
// one reference-counted lifetime so VENC cannot tear SYS down while audio is
// still active (or vice versa).
bool acquire_rockit_system();
void release_rockit_system();

}  // namespace aico
