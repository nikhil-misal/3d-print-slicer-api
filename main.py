from fastapi import FastAPI, UploadFile, File, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from stl import mesh
import tempfile
import os

app = FastAPI(
    title="3D Print Slicer API",
    version="1.0.0"
)

# Shopify website से API access allow करना
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

# Material densities in g/cm³
MATERIAL_DENSITIES = {
    "PLA": 1.24,
    "PLA+": 1.24,
    "PETG": 1.27,
    "ABS": 1.04,
    "TPU": 1.21,
    "NYLON": 1.15,
}


@app.get("/")
def home():
    return {
        "status": "online",
        "message": "3D Print Slicer API is running"
    }


@app.post("/analyze")
async def analyze_stl(
    file: UploadFile = File(...),
    material: str = "PLA",
    infill: float = 20
):
    # Only STL files
    if not file.filename.lower().endswith(".stl"):
        raise HTTPException(
            status_code=400,
            detail="Only STL files are supported"
        )

    # Validate material
    material_key = material.upper()

    if material_key not in MATERIAL_DENSITIES:
        raise HTTPException(
            status_code=400,
            detail=f"Unsupported material. Use: {list(MATERIAL_DENSITIES.keys())}"
        )

    # Validate infill
    if infill < 0 or infill > 100:
        raise HTTPException(
            status_code=400,
            detail="Infill must be between 0 and 100"
        )

    temp_path = None

    try:
        # Save uploaded STL temporarily
        with tempfile.NamedTemporaryFile(
            delete=False,
            suffix=".stl"
        ) as temp_file:
            temp_path = temp_file.name
            content = await file.read()
            temp_file.write(content)

        # Load STL model
        model = mesh.Mesh.from_file(temp_path)

        # Calculate volume
        volume_mm3, _, _ = model.get_mass_properties()

        volume_mm3 = abs(float(volume_mm3))
        volume_cm3 = volume_mm3 / 1000

        # Calculate dimensions
        min_values = model.vectors.min(axis=(0, 1))
        max_values = model.vectors.max(axis=(0, 1))

        dimensions_mm = max_values - min_values

        length = float(dimensions_mm[0])
        width = float(dimensions_mm[1])
        height = float(dimensions_mm[2])

        # Material density
        density = MATERIAL_DENSITIES[material_key]

        # Solid model weight
        solid_weight_g = volume_cm3 * density

        # Estimated print weight
        # Includes approximate shell contribution
        infill_factor = (infill / 100) * 0.70
        shell_factor = 0.25

        material_usage_factor = min(
            1.0,
            shell_factor + infill_factor
        )

        estimated_weight_g = (
            solid_weight_g * material_usage_factor
        )

        return {
            "success": True,
            "file_name": file.filename,
            "material": material_key,
            "infill_percent": infill,

            "volume": {
                "mm3": round(volume_mm3, 2),
                "cm3": round(volume_cm3, 2)
            },

            "dimensions": {
                "x_mm": round(length, 2),
                "y_mm": round(width, 2),
                "z_mm": round(height, 2)
            },

            "density_g_cm3": density,

            "solid_weight_g": round(
                solid_weight_g,
                2
            ),

            "estimated_print_weight_g": round(
                estimated_weight_g,
                2
            ),

            "status": "model_analyzed"
        }

    except Exception as e:
        raise HTTPException(
            status_code=500,
            detail=f"Could not process STL file: {str(e)}"
        )

    finally:
        # Delete temporary file
        if temp_path and os.path.exists(temp_path):
            os.remove(temp_path)
