// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The tool service: what an agent does to files and the web, natively in
//! FeOS and confined to a container's root filesystem, so that it works
//! whatever the image contains, even a scratch image.

mod fs;
mod tools;

pub use tools::ToolApiHandler;
